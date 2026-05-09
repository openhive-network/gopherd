package state

import (
	"errors"

	"github.com/tidwall/btree"
)

// Session is an in-memory overlay over Database that buffers reads/writes
// until Commit. The overlay maps physical keys to entries; reads consult
// the overlay first and only fall through to Pebble when the key isn't in
// the buffer. Writes update the overlay alone — no Pebble writes happen
// until Commit. Undo simply drops the overlay.
//
// Replay benefits hugely from this design: hived's chainbase tracks
// per-modification undo log entries for every block-apply, which is the
// largest single source of replay overhead. We elide that bookkeeping
// because replay is monotonically forward; for the rare reorg path we
// will add a reverse-undo log later (see plan).
type Session struct {
	db      *Database
	// snapshotGen is incremented every Snapshot() and reset to 0 on
	// Commit/Undo. Overlay entries record the gen at which they were
	// allocated; Put can safely mutate an entry's value buffer in
	// place IFF the entry's gen equals the current snapshotGen
	// (otherwise an active Savepoint may be aliasing the buffer and
	// in-place mutation would corrupt the snapshot's view).
	snapshotGen uint32
	overlay     map[string]overlayEntry
	// overlayKeys is a sorted-by-physical-key btree of every key written
	// via Put/Delete this Session. Range walks this tree from the prefix
	// lower-bound to its exclusive upper-bound, looking up entries in
	// the overlay map. Without this, Range scanned the full overlay map
	// (O(N) per call) — fatal at the HF7 cashout cascade (block
	// 2,889,019) which processes thousands of comments per block, where
	// each distributeCuratorPool/deleteCommentVotes call would re-scan a
	// growing overlay.
	overlayKeys *btree.BTreeG[string]
	// dirtyKeys is the append-only list of overlay keys touched this
	// Session. Commit walks this slice (cheap iteration, cache-friendly)
	// rather than ranging the overlay map (slow swisstable iter, ~16%
	// of replay CPU). The slice may contain duplicates — both processed
	// at commit; final write wins, which matches overlay semantics.
	dirtyKeys []string
	// scratch is reused by EncodeKeyInto in helper methods; per-Session,
	// not safe for concurrent use.
	scratch []byte
	// SerScratch is a per-Session serialization buffer that domain Put
	// helpers may reuse across calls. Session.Put copies the value, so
	// callers can re-use the same slice for the next Append. Avoids
	// per-Put allocation churn from the binary.AppendUintXX growslice
	// path (one of the largest replay allocators).
	SerScratch []byte
	// iter is a pooled backend iterator reused across Range calls within
	// this Session. Lazily allocated on first Range; closed by Commit or
	// Undo. Reusing the iterator avoids the per-call NewIter cost and is
	// safe because a Session never writes to the backend until Commit
	// (so the snapshot pinned at iter creation is always consistent
	// with what subsequent overlay-or-backend reads expect).
	iter Iter
	// readCache memoizes Pebble Get results within this Session — the
	// same row is often re-read several times per block (FeedHistory,
	// DGPO, WitnessSchedule, hot witness rows). Invalidated whenever
	// the overlay receives a Put/Delete for the same key.
	readCache map[string][]byte
	// Aux is a per-Session typed-object cache used by domain code to
	// avoid re-deserializing hot singleton rows (FeedHistory, DGPO,
	// WitnessSchedule). Keys are arbitrary strings chosen by callers;
	// values are arbitrary; nil-safe via map literal access.
	Aux map[string]any
	// preCommit holds callbacks invoked just before the overlay is
	// flushed to the backend. Used by domain code that lazy-writes hot
	// singletons (DGPO) — the struct is held in Aux during the block,
	// mutated in place, and serialized once at commit time instead of
	// on every Put.
	preCommit []func()
	// mistrustDBAux is set by Restore. While true, parsed-object
	// accessors (GetWitness, etc.) skip the cross-Session DatabaseAux
	// cache and fall through to overlay/backend — overlay holds the
	// post-Restore truth, but DatabaseAux still mirrors what was
	// committed before this Session started, which is now stale
	// relative to the surviving overlay state.
	mistrustDBAux bool
	// undoLog records overlay pre-images for every Put/Delete made
	// while at least one Savepoint is OUTSTANDING (incremented by
	// Snapshot, decremented by Restore). Restore replays the relevant
	// undo-log slice in reverse to revert. Collapses the prior
	// O(overlay-size) shallow-copy per Snapshot into O(tx-edits) —
	// the dominant 3.5M+ cliff cost (Snapshot was 55% of CPU at
	// block 3.6M).
	//
	// snapshotDepth is decremented on Restore but NOT on tx-success
	// (callers don't notify us of successful txs). The simplest
	// pragmatic outcome: after the first successful tx, depth stays
	// > 0 and subsequent Puts log entries we'll never use, until
	// Commit resets. The cost is small in practice (~one append
	// per Put after the first tx); the alternative — full undo
	// always — was measurably slower on a clean 3.0M→3.5M run.
	undoLog       []sessionUndoEntry
	snapshotDepth int
}

// sessionUndoEntry captures the overlay state for one key BEFORE a
// Put/Delete overwrote it. `had` distinguishes "key didn't exist in
// overlay (undo by deleting both overlay + overlayKeys)" from "key
// existed with prev value (undo by restoring overlay[key]=prev)".
type sessionUndoEntry struct {
	key  string
	prev overlayEntry
	had  bool
}

// RegisterPreCommit schedules fn to run just before this Session's
// overlay is flushed to the backend. Callbacks fire in registration
// order. Used by lazy-write helpers (PutDGPO) to defer serialization
// to commit time.
func (s *Session) RegisterPreCommit(fn func()) {
	s.preCommit = append(s.preCommit, fn)
}


// AuxInvalidate drops a cached parsed object. Called by domain Put
// helpers when they overwrite the underlying row.
func (s *Session) AuxInvalidate(key string) {
	if s.Aux != nil {
		delete(s.Aux, key)
	}
}

// DatabaseAux returns a cross-Session parsed-object cache entry.
func (s *Session) DatabaseAux(key string) (any, bool) {
	v, ok := s.db.Aux[key]
	return v, ok
}

// DatabaseAuxStore puts a parsed object into the cross-Session cache.
func (s *Session) DatabaseAuxStore(key string, v any) {
	s.db.Aux[key] = v
}

// DatabaseAuxInvalidate drops a cross-Session parsed-object cache entry.
func (s *Session) DatabaseAuxInvalidate(key string) {
	s.db.AuxInvalidate(key)
}

// HasOverlayWrite reports whether this Session has written to (table,
// index) in its overlay. Used by domain accessors that consult a
// cross-Session parsed cache: if the row was just written here, the
// cache is stale relative to the overlay and must be bypassed.
func (s *Session) HasOverlayWrite(table TableID, index IndexID) bool {
	if s.overlayKeys == nil || s.overlayKeys.Len() == 0 {
		return false
	}
	prefix := EncodePrefix(table, index)
	upper := nextPrefix(prefix)
	hit := false
	s.overlayKeys.Ascend(string(prefix), func(k string) bool {
		if k >= string(upper) {
			return false
		}
		hit = true
		return false
	})
	return hit
}

// MistrustDatabaseAux reports whether parsed-object accessors should
// skip the cross-Session DatabaseAux cache for this Session. Set by
// Restore — after a tx rollback the Session's overlay holds values
// that are NEWER than DatabaseAux (which mirrors committed state),
// so the accessors must consult overlay/backend directly until next
// Commit clears the flag.
func (s *Session) MistrustDatabaseAux() bool { return s.mistrustDBAux }

// Database returns the underlying *Database. Domain code uses this to
// fetch typed in-memory subsystems (objects.WitnessStore, …) that bypass
// the K/V overlay path entirely.
func (s *Session) Database() *Database { return s.db }

// GetExpHint forwards to the underlying Database.
func (s *Session) GetExpHint(table TableID, index IndexID) (uint32, bool) {
	return s.db.GetExpHint(table, index)
}

// SetExpHint forwards to the underlying Database.
func (s *Session) SetExpHint(table TableID, index IndexID, time uint32) {
	s.db.SetExpHint(table, index, time)
}

// NudgeExpHint forwards to the underlying Database.
func (s *Session) NudgeExpHint(table TableID, index IndexID, newTime uint32) {
	s.db.NudgeExpHint(table, index, newTime)
}

type overlayEntry struct {
	value     []byte
	tombstone bool
	// gen is the snapshotGen at which this value buffer was allocated
	// (or last mutated in place). Put may reuse the buffer in place
	// when entry.gen == s.snapshotGen — guaranteeing no live Savepoint
	// aliases it. After a Snapshot bumps the gen counter, any
	// subsequent Put-to-same-key falls through to a fresh allocation.
	gen uint32
}

// NewSession returns a fresh in-memory session over db.
func NewSession(db *Database) *Session {
	return &Session{
		db:          db,
		overlay:     make(map[string]overlayEntry, 64),
		overlayKeys: newOverlayKeyTree(),
		dirtyKeys:   make([]string, 0, 256),
		undoLog:     make([]sessionUndoEntry, 0, 256),
	}
}

// newOverlayKeyTree builds the sorted physical-key btree used to drive
// Session.Range. Lock-free (single-goroutine Session use), degree 32 to
// match memBackend.
func newOverlayKeyTree() *btree.BTreeG[string] {
	return btree.NewBTreeGOptions[string](
		func(a, b string) bool { return a < b },
		btree.Options{Degree: 32, NoLocks: true},
	)
}

// Get returns the latest value for (table, index, body) — checking the
// overlay first, then the Database batch overlay (pending writes that
// haven't yet flushed to the backend, only used when BatchSize>1),
// then the per-Session read cache, then the backend.
//
// The returned []byte aliases internal storage and MUST NOT be modified
// by callers. The contract holds for the lifetime of the Session: any
// subsequent Put/Delete to the same key invalidates the cache and the
// next Get returns a fresh slice. Decoders that walk bytes read-only
// (the common case for Read* helpers) are safe.
func (s *Session) Get(table TableID, index IndexID, body []byte) ([]byte, error) {
	s.scratch = EncodeKeyInto(s.scratch, table, index, body)
	if e, ok := s.overlay[string(s.scratch)]; ok {
		if e.tombstone {
			return nil, ErrNotFound
		}
		return e.value, nil
	}
	if e, ok := s.db.batchGet(string(s.scratch)); ok {
		if e.tombstone {
			return nil, ErrNotFound
		}
		return e.value, nil
	}
	if v, ok := s.readCache[string(s.scratch)]; ok {
		return v, nil
	}
	v, err := s.db.rawGet(s.scratch)
	if err != nil {
		return nil, err
	}
	if s.readCache == nil {
		s.readCache = make(map[string][]byte, 16)
	}
	s.readCache[string(s.scratch)] = v
	return v, nil
}

// Has reports whether (table, index, body) exists.
func (s *Session) Has(table TableID, index IndexID, body []byte) (bool, error) {
	s.scratch = EncodeKeyInto(s.scratch, table, index, body)
	if e, ok := s.overlay[string(s.scratch)]; ok {
		return !e.tombstone, nil
	}
	if e, ok := s.db.batchGet(string(s.scratch)); ok {
		return !e.tombstone, nil
	}
	if _, ok := s.readCache[string(s.scratch)]; ok {
		return true, nil
	}
	return s.db.rawHas(s.scratch)
}

// Put stores value for (table, index, body) in the overlay. The slices are
// copied; callers may reuse their buffers.
//
// Allocation: when the same key is Put multiple times in a block (the
// common case for DGPO — 5+ writes per block from per-block hooks),
// the overlay entry's value buffer is REUSED in place when its
// capacity is sufficient, eliminating the per-Put make+copy. Only the
// first Put-per-key allocates; subsequent Puts to the same key amortize
// to a copy into the existing slot.
func (s *Session) Put(table TableID, index IndexID, body, value []byte) {
	s.scratch = EncodeKeyInto(s.scratch, table, index, body)
	fullKey := string(s.scratch)
	// Reuse-in-place: if the overlay already holds a (non-tombstoned)
	// entry for this key with sufficient capacity, copy into the
	// existing buffer. The Get contract documents that returned slices
	// are invalidated by subsequent Put to the same key, so aliasing
	// callers who held the prior slice are already responsible for
	// re-fetching — the in-place mutation matches that contract.
	existing, hadEntry := s.overlay[fullKey]
	var cv []byte
	if len(value) > 0 {
		if hadEntry && !existing.tombstone && existing.gen == s.snapshotGen && cap(existing.value) >= len(value) {
			cv = existing.value[:len(value)]
			copy(cv, value)
		} else {
			cv = make([]byte, len(value))
			copy(cv, value)
		}
	}
	if !hadEntry {
		s.overlayKeys.Set(fullKey)
	}
	if s.snapshotDepth > 0 {
		s.undoLog = append(s.undoLog, sessionUndoEntry{key: fullKey, prev: existing, had: hadEntry})
	}
	s.overlay[fullKey] = overlayEntry{value: cv, gen: s.snapshotGen}
	// Append to dirtyKeys, deduping consecutive writes to the same key.
	// Catches the common DGPO read-modify-write pattern where the same
	// key gets Put 2-5× in a row from adjacent per-block hooks. Without
	// this, commitDirty did 30%+ of CPU at chain head re-Setting the
	// same btree key on each duplicate.
	if n := len(s.dirtyKeys); n == 0 || s.dirtyKeys[n-1] != fullKey {
		s.dirtyKeys = append(s.dirtyKeys, fullKey)
	}
	if s.readCache != nil {
		delete(s.readCache, fullKey)
	}
}

// Delete marks (table, index, body) as deleted in the overlay. Subsequent
// Get calls return ErrNotFound until the deletion is undone.
func (s *Session) Delete(table TableID, index IndexID, body []byte) {
	s.scratch = EncodeKeyInto(s.scratch, table, index, body)
	fullKey := string(s.scratch)
	prev, exists := s.overlay[fullKey]
	if !exists {
		s.overlayKeys.Set(fullKey)
	}
	if s.snapshotDepth > 0 {
		s.undoLog = append(s.undoLog, sessionUndoEntry{key: fullKey, prev: prev, had: exists})
	}
	s.overlay[fullKey] = overlayEntry{tombstone: true}
	// Same consecutive-key dedup as Put — Delete-then-Delete to the same
	// key is rare but handle uniformly.
	if n := len(s.dirtyKeys); n == 0 || s.dirtyKeys[n-1] != fullKey {
		s.dirtyKeys = append(s.dirtyKeys, fullKey)
	}
	if s.readCache != nil {
		delete(s.readCache, fullKey)
	}
}

// Commit flushes the overlay atomically to the backend (or, when
// Database.BatchSize>1, into the Database batch overlay until the
// batch boundary is reached). After Commit the overlay is reset and
// the Session can be reused for the next block.
//
// sync controls whether the underlying write is fsync'd (Pebble only).
// For replay we pass false — durability comes from periodic flushes;
// for live sync / witness production callers should pass true. With
// batching enabled, sync only takes effect on the actual backend
// write fired at the batch boundary.
func (s *Session) Commit(sync bool) error {
	for _, fn := range s.preCommit {
		fn()
	}
	s.preCommit = s.preCommit[:0]
	if s.iter != nil {
		s.iter.Close()
		s.iter = nil
	}
	if len(s.overlay) == 0 {
		clear(s.readCache)
		clear(s.Aux)
		s.overlayKeys.Clear()
		s.dirtyKeys = s.dirtyKeys[:0]
		s.undoLog = s.undoLog[:0]
		s.snapshotDepth = 0
		s.mistrustDBAux = false
		s.snapshotGen = 0
		s.db.commitAll()
		// An empty Commit still bumps the batch boundary so a
		// caller relying purely on FlushBatch semantics doesn't
		// silently delay a pending flush by one block.
		if s.db.BatchSize > 1 && s.db.pendingBlocks > 0 && s.db.pendingBlocks >= s.db.BatchSize {
			return s.db.FlushBatch(sync)
		}
		return nil
	}
	if s.db.BatchSize > 1 {
		s.db.mergeSessionIntoBatch(s.overlay, s.dirtyKeys)
		clear(s.overlay)
		clear(s.readCache)
		clear(s.Aux)
		s.overlayKeys.Clear()
		s.dirtyKeys = s.dirtyKeys[:0]
		s.undoLog = s.undoLog[:0]
		s.snapshotDepth = 0
		s.mistrustDBAux = false
		s.snapshotGen = 0
		s.db.commitAll()
		if s.db.pendingBlocks >= s.db.BatchSize {
			return s.db.FlushBatch(sync)
		}
		return nil
	}
	if err := s.db.backend.commitDirty(s.overlay, s.dirtyKeys, sync); err != nil {
		return err
	}
	clear(s.overlay)
	clear(s.readCache)
	clear(s.Aux)
	s.overlayKeys.Clear()
	s.dirtyKeys = s.dirtyKeys[:0]
	s.undoLog = s.undoLog[:0]
	s.snapshotDepth = 0
	s.mistrustDBAux = false
	s.snapshotGen = 0
	s.db.commitAll()
	return nil
}

// Undo discards the overlay without writing to Pebble.
func (s *Session) Undo() {
	if s.iter != nil {
		s.iter.Close()
		s.iter = nil
	}
	clear(s.overlay)
	clear(s.readCache)
	clear(s.Aux)
	s.overlayKeys.Clear()
	s.preCommit = s.preCommit[:0]
	s.dirtyKeys = s.dirtyKeys[:0]
	s.undoLog = s.undoLog[:0]
	s.snapshotDepth = 0
	s.mistrustDBAux = false
	s.snapshotGen = 0
	s.db.undoAll()
}

// Empty reports whether the session has any pending modifications.
func (s *Session) Empty() bool { return len(s.overlay) == 0 }

// Savepoint is an opaque snapshot of the Session's overlay at the
// moment of capture. Used to roll back the per-tx slice of writes
// when a transaction fails mid-block, without affecting prior txs in
// the same block.
//
// In hived this is the chainbase undo_session pushed at
// `_db.start_undo_session(true)` for every push_transaction inside
// apply_block. Failure throws and the session pop unwinds. We mirror
// it via a per-Session undo log: Snapshot only records the current
// undo-log length (and dirtyKeys/preCommit lengths); Restore replays
// undo entries in reverse to revert. The previous design did a full
// O(overlay-size) shallow copy per Snapshot, which became 55% of CPU
// at block 3.6M as overlay grew with per-block edits.
type Savepoint struct {
	undoLogLen   int
	dirtyKeysLen int
	preCommitLen int
	// subsystemPoints captures the SnapshotPoint() of each registered
	// state.Database subsystem (typed in-memory containers like
	// objects.WitnessStore). Restore replays each subsystem's undo
	// log down to its captured point so chainbase-style stores roll
	// back in lock-step with the K/V overlay.
	subsystemPoints []int
}

// Snapshot captures the current overlay position so a later Restore
// can revert any Put/Delete made between Snapshot and Restore.
//
// Cost: O(1) for the K/V overlay (just a length read). Subsystem
// snapshots remain O(num-registered-subsystems).
//
// While snapshotDepth > 0, every subsequent Put/Delete in this
// Session logs a pre-image into s.undoLog. Restore decrements depth;
// per-block hooks that fire BEFORE any tx (when depth==0) pay zero
// cost. Hooks that fire AFTER all txs still pay because tx-success
// does not decrement depth.
func (s *Session) Snapshot() Savepoint {
	s.snapshotDepth++
	subPts := s.db.snapshotAll(make([]int, 0, len(s.db.subsystems)))
	// Bump snapshotGen so any subsequent Put cannot reuse-in-place a
	// value buffer aliased by this Savepoint's overlay copy.
	s.snapshotGen++
	return Savepoint{
		undoLogLen:      len(s.undoLog),
		dirtyKeysLen:    len(s.dirtyKeys),
		preCommitLen:    len(s.preCommit),
		subsystemPoints: subPts,
	}
}

// Restore reverts the overlay to the state captured by sp. Any
// Put/Delete since the snapshot is discarded by replaying undo
// entries in reverse from len(s.undoLog)-1 down to sp.undoLogLen.
// The typed-subsystem stores roll back via their own undo logs to
// the captured points.
//
// Per-Session caches (readCache, Aux) and pre-commit hooks beyond
// preCommitLen are intentionally NOT cleared — they may carry values
// from the rolled-back tx, but the pre-typed-store replay was tuned
// against that historical leakage, so cv-cleanup / vote-rshares
// accounting matches C++ only when those caches survive Restore.
// Tightening them broke acidsun's HBD at block 2,904,813 (3 sat short).
func (s *Session) Restore(sp Savepoint) {
	for i := len(s.undoLog) - 1; i >= sp.undoLogLen; i-- {
		e := s.undoLog[i]
		if e.had {
			s.overlay[e.key] = e.prev
		} else {
			delete(s.overlay, e.key)
			s.overlayKeys.Delete(e.key)
		}
		// Invalidate any read-cache entry — overlay just reverted.
		if s.readCache != nil {
			delete(s.readCache, e.key)
		}
	}
	s.undoLog = s.undoLog[:sp.undoLogLen]
	if sp.dirtyKeysLen >= 0 && sp.dirtyKeysLen <= len(s.dirtyKeys) {
		s.dirtyKeys = s.dirtyKeys[:sp.dirtyKeysLen]
	}
	// Truncate preCommit hooks back to snapshot length. Hooks added by
	// the rolled-back tx would otherwise still fire at commit and
	// serialize stale state.
	if sp.preCommitLen >= 0 && sp.preCommitLen <= len(s.preCommit) {
		s.preCommit = s.preCommit[:sp.preCommitLen]
	}
	if s.snapshotDepth > 0 {
		s.snapshotDepth--
	}
	// Invalidate the dgpo Aux cache: PutDGPO writes the overlay
	// directly so Restore reverted the actual state, but the parsed
	// dgpo cached in s.Aux still holds the failed-tx's value. Clearing
	// it forces the next GetDGPO to re-parse from the (now-reverted)
	// overlay, returning the correct PRE-tx state. Only the dgpo entry
	// is cleared — readCache and other Aux entries are preserved per
	// the historical tuning (cv-cleanup / vote-rshares accounting at
	// acidsun's HBD block 2,904,813 was tuned against full Aux survival
	// across Restore, so we narrow the invalidation to dgpo only).
	if s.Aux != nil {
		delete(s.Aux, "dgpo")
	}
	s.db.restoreAll(sp.subsystemPoints)
}

// Range iterates entries in [table, index] whose body bytes start with
// prefix. The iterator visits overlay entries AND committed entries,
// merging them in lexicographic order with overlay taking precedence.
//
// fn is called with (body, value) for each entry. Returning false from fn
// stops iteration. Returning a non-nil error from fn is propagated.
//
// Implementation note: this is the simplest correct version — it
// concatenates overlay + Pebble entries, sorts, and dedups. For workloads
// with very large overlays we'll want a proper merging iterator; for now
// the overlay is bounded to one block's worth of edits.
func (s *Session) Range(table TableID, index IndexID, prefix []byte, fn func(body, value []byte) (bool, error)) error {
	lower := EncodeKey(table, index, prefix)
	// Upper bound for a prefix scan is the lexicographically next byte
	// string after `lower`. The previous implementation `lower + [0xff]`
	// is wrong: any stored key strictly longer than `lower + [0xff]`
	// (which is essentially every stored row) compares as `> upper`
	// under bytes.Compare's "longer-when-prefix-equal-is-greater"
	// rule, so Pebble's exclusive UpperBound dropped them. Mainnet
	// limit-order matching (and every other secondary-index prefix
	// scan) silently saw zero candidates as a result.
	upper := nextPrefix(lower)

	// Pooled Pebble iterator: allocate once per Session, reuse via
	// SetBounds for all subsequent Ranges. Avoids the per-call NewIter
	// cost (which dominated CPU under the per-block hook hot path).
	if s.iter == nil {
		it, err := s.db.rawIter(lower, upper)
		if err != nil {
			return err
		}
		s.iter = it
	} else {
		s.iter.SetBounds(lower, upper)
	}
	iter := s.iter

	emit := func(body, value []byte) error {
		ok, err := fn(body, value)
		if err != nil {
			return err
		}
		if !ok {
			return errStopIteration
		}
		return nil
	}

	// Merge backend + overlay entries in physical-key (sort) order so the
	// caller sees the same ordering chainbase does: every limit-order
	// matching pass needs an opposite-side order placed THIS BLOCK to
	// be considered alongside committed orders, sorted by their actual
	// key. Iterate both cursors in lockstep:
	//
	//   - backend: forward iteration over the prefix range
	//   - overlay: ascending btree walk from `lower` (stops at `upper`)
	//
	// On each step emit the smaller key (overlay wins on tie, modeling
	// "overlay overrides committed"). Tombstones in the overlay suppress
	// the corresponding emit but never the overlay's smaller-key
	// advancement.
	upperStr := string(upper)
	// Skip the overlay-iterator allocation entirely when the overlay
	// is empty (the common case: most per-block hooks Range tables
	// they don't write to). overlayKeys.Iter() escapes to the heap
	// (stack-bound iterStackItemG slice) and dominated Range alloc
	// pressure (364MB / 1M blocks).
	hasOverlay := s.overlayKeys.Len() > 0
	hasBatch := s.db.batchOverlayKeys != nil && s.db.batchOverlayKeys.Len() > 0
	if !hasOverlay && !hasBatch {
		// Backend-only fast path: no overlay merge.
		iter.First()
		for iter.Valid() {
			k := iter.Key()
			v, err := iter.ValueAndErr()
			if err != nil {
				return err
			}
			body := k[keyPrefixLen:]
			if err := emit(body, v); err != nil {
				if errors.Is(err, errStopIteration) {
					return nil
				}
				return err
			}
			iter.Next()
		}
		return iter.Error()
	}
	if !hasBatch {
		overlayItr := s.overlayKeys.Iter()
		overlayValid := overlayItr.Seek(string(lower)) && overlayItr.Item() < upperStr
		iter.First()
		rangeErr := s.rangeMerge(iter, &overlayItr, &overlayValid, upperStr, emit)
		overlayItr.Release()
		return rangeErr
	}
	// 3-way merge: session overlay > batch overlay > backend.
	var overlayItr btree.IterG[string]
	overlayValid := false
	if hasOverlay {
		overlayItr = s.overlayKeys.Iter()
		overlayValid = overlayItr.Seek(string(lower)) && overlayItr.Item() < upperStr
	}
	batchItr := s.db.batchOverlayKeys.Iter()
	batchValid := batchItr.Seek(string(lower)) && batchItr.Item() < upperStr
	iter.First()
	rangeErr := s.rangeMerge3(iter, &overlayItr, &overlayValid, &batchItr, &batchValid, upperStr, emit)
	if hasOverlay {
		overlayItr.Release()
	}
	batchItr.Release()
	return rangeErr
}

// rangeMerge3 merges session overlay (highest precedence) + batch
// overlay (medium) + backend (lowest) in lexicographic key order.
// Session overlay tombstones suppress the corresponding emit; batch
// overlay tombstones suppress the backend row only when no session
// override exists for the same key.
func (s *Session) rangeMerge3(
	iter Iter,
	overlayItr *btree.IterG[string], overlayValid *bool,
	batchItr *btree.IterG[string], batchValid *bool,
	upperStr string,
	emit func(body, value []byte) error,
) error {
	for {
		backendValid := iter.Valid()
		if !backendValid && !*overlayValid && !*batchValid {
			break
		}
		// Find the smallest pending key across the three sources.
		var minKey string
		first := true
		if backendValid {
			minKey = string(iter.Key())
			first = false
		}
		if *batchValid {
			bk := batchItr.Item()
			if first || bk < minKey {
				minKey = bk
				first = false
			}
		}
		if *overlayValid {
			ok := overlayItr.Item()
			if first || ok < minKey {
				minKey = ok
			}
		}

		// Resolve the value at minKey by precedence:
		//   1. session overlay (if it owns minKey)
		//   2. batch overlay (if it owns minKey)
		//   3. backend (if it owns minKey)
		// Advance every cursor that was sitting on minKey.
		var emitVal []byte
		emitTombstone := false
		emitFromAny := false

		if *overlayValid && overlayItr.Item() == minKey {
			e := s.overlay[minKey]
			emitVal = e.value
			emitTombstone = e.tombstone
			emitFromAny = true
			if !overlayItr.Next() {
				*overlayValid = false
			} else if overlayItr.Item() >= upperStr {
				*overlayValid = false
			}
		}
		if *batchValid && batchItr.Item() == minKey {
			if !emitFromAny {
				e := s.db.batchOverlay[minKey]
				emitVal = e.value
				emitTombstone = e.tombstone
				emitFromAny = true
			}
			if !batchItr.Next() {
				*batchValid = false
			} else if batchItr.Item() >= upperStr {
				*batchValid = false
			}
		}
		if backendValid && string(iter.Key()) == minKey {
			if !emitFromAny {
				v, err := iter.ValueAndErr()
				if err != nil {
					return err
				}
				emitVal = v
				emitFromAny = true
			}
			iter.Next()
		}

		if emitTombstone || !emitFromAny {
			continue
		}
		body := []byte(minKey)[keyPrefixLen:]
		if err := emit(body, emitVal); err != nil {
			if errors.Is(err, errStopIteration) {
				return nil
			}
			return err
		}
	}
	if err := iter.Error(); err != nil {
		return err
	}
	return nil
}

// rangeMerge implements the merge loop used by Range when both backend
// and overlay sides may have entries. Extracted so Range can release
// the overlay iterator promptly after the loop returns.
func (s *Session) rangeMerge(iter Iter, overlayItr *btree.IterG[string], overlayValid *bool, upperStr string, emit func(body, value []byte) error) error {
	for {
		backendValid := iter.Valid()
		if !backendValid && !*overlayValid {
			break
		}
		// Decide which side to consume next.
		consumeOverlay := false
		if backendValid && *overlayValid {
			bk := iter.Key()
			ok := overlayItr.Item()
			cmp := compareBytes(bk, []byte(ok))
			// equal keys → overlay overrides backend; consume overlay.
			consumeOverlay = cmp >= 0
		} else if !backendValid {
			consumeOverlay = true
		}

		if consumeOverlay {
			ok := overlayItr.Item()
			e := s.overlay[ok]
			// If both sides are at the same key, advance backend too.
			if backendValid && compareBytes(iter.Key(), []byte(ok)) == 0 {
				iter.Next()
			}
			// Advance overlay cursor.
			if !overlayItr.Next() {
				*overlayValid = false
			} else if overlayItr.Item() >= upperStr {
				*overlayValid = false
			}
			if e.tombstone {
				continue
			}
			body := []byte(ok)[keyPrefixLen:]
			if err := emit(body, e.value); err != nil {
				if errors.Is(err, errStopIteration) {
					return nil
				}
				return err
			}
			continue
		}

		// consumeBackend
		k := iter.Key()
		v, err := iter.ValueAndErr()
		if err != nil {
			return err
		}
		body := k[keyPrefixLen:]
		if err := emit(body, v); err != nil {
			if errors.Is(err, errStopIteration) {
				return nil
			}
			return err
		}
		iter.Next()
	}
	if err := iter.Error(); err != nil {
		return err
	}
	return nil
}

var errStopIteration = errors.New("stop iteration")

func inRange(key, lower, upper []byte) bool {
	return compareBytes(key, lower) >= 0 && compareBytes(key, upper) < 0
}

// nextPrefix returns the smallest byte string that compares strictly
// greater than every byte string starting with `prefix`. Increments
// the last non-0xff byte and truncates trailing 0xffs; if every byte
// is 0xff, returns nil to signal "no upper bound" (Pebble accepts a
// nil UpperBound as +inf).
func nextPrefix(prefix []byte) []byte {
	out := append([]byte(nil), prefix...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xff {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}

func hasPrefix(s string, prefix []byte) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i, b := range prefix {
		if s[i] != b {
			return false
		}
	}
	return true
}

func compareBytes(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return len(a) - len(b)
}

