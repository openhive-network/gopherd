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
	overlay map[string]overlayEntry
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
}

// NewSession returns a fresh in-memory session over db.
func NewSession(db *Database) *Session {
	return &Session{
		db:          db,
		overlay:     make(map[string]overlayEntry, 64),
		overlayKeys: newOverlayKeyTree(),
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
// overlay first, then the per-Session read cache, then the backend.
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
	if _, ok := s.readCache[string(s.scratch)]; ok {
		return true, nil
	}
	return s.db.rawHas(s.scratch)
}

// Put stores value for (table, index, body) in the overlay. The slices are
// copied; callers may reuse their buffers.
func (s *Session) Put(table TableID, index IndexID, body, value []byte) {
	s.scratch = EncodeKeyInto(s.scratch, table, index, body)
	// Secondary-index writes ship a zero-length value (the sort key
	// lives in the body); skip the per-call make+copy in that case so
	// each index update only allocates the overlay key string.
	var cv []byte
	if len(value) > 0 {
		cv = make([]byte, len(value))
		copy(cv, value)
	}
	fullKey := string(s.scratch)
	if _, exists := s.overlay[fullKey]; !exists {
		s.overlayKeys.Set(fullKey)
	}
	s.overlay[fullKey] = overlayEntry{value: cv}
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
	if _, exists := s.overlay[fullKey]; !exists {
		s.overlayKeys.Set(fullKey)
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

// Commit flushes the overlay atomically to the backend. After Commit
// the overlay is reset and the Session can be reused for the next block.
//
// sync controls whether the underlying write is fsync'd (Pebble only).
// For replay we pass false — durability comes from periodic flushes;
// for live sync / witness production callers should pass true.
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
		s.mistrustDBAux = false
		s.db.commitAll()
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
	s.mistrustDBAux = false
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
	s.mistrustDBAux = false
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
// apply_block. Failure throws and the session pop unwinds. Our model
// is a shallow map snapshot — far cheaper than a per-modification
// reverse log for the common case where tx failure is rare.
type Savepoint struct {
	overlay      map[string]overlayEntry
	keys         []string
	dirtyKeys    []string
	preCommitLen int
	// subsystemPoints captures the SnapshotPoint() of each registered
	// state.Database subsystem (typed in-memory containers like
	// objects.WitnessStore). Restore replays each subsystem's undo
	// log down to its captured point so chainbase-style stores roll
	// back in lock-step with the K/V overlay.
	subsystemPoints []int
}

// Snapshot captures the current overlay state. Subsequent Put/Delete
// calls modify the live overlay; Restore(sp) reverts to the snapshot.
//
// Cost: O(len(overlay)) — a shallow copy of the map. The values slice
// is shared, since Put always allocates a fresh slice.
//
// preCommitLen records the count of registered pre-commit hooks at
// snapshot time; Restore truncates the slice back so any hooks added
// by the rolled-back tx don't fire at commit (their captured lazy
// state is now invalid).
func (s *Session) Snapshot() Savepoint {
	cp := make(map[string]overlayEntry, len(s.overlay))
	for k, v := range s.overlay {
		cp[k] = v
	}
	keys := make([]string, 0, s.overlayKeys.Len())
	s.overlayKeys.Scan(func(k string) bool { keys = append(keys, k); return true })
	dk := append([]string(nil), s.dirtyKeys...)
	subPts := s.db.snapshotAll(make([]int, 0, len(s.db.subsystems)))
	return Savepoint{
		overlay:         cp,
		keys:            keys,
		dirtyKeys:       dk,
		preCommitLen:    len(s.preCommit),
		subsystemPoints: subPts,
	}
}

// Restore reverts the overlay to the state captured by sp. Any
// Put/Delete since the snapshot is discarded. Per-Session caches
// (readCache, Aux) are also dropped — they may have absorbed values
// from the now-reverted overlay, and the cost of rebuilding them is
// dwarfed by the rarity of this path (tx failure). Pre-commit hooks
// added since the snapshot are dropped too — their captured lazy
// state was undone with the overlay.
func (s *Session) Restore(sp Savepoint) {
	cp := make(map[string]overlayEntry, len(sp.overlay))
	for k, v := range sp.overlay {
		cp[k] = v
	}
	s.overlay = cp
	s.overlayKeys.Clear()
	for _, k := range sp.keys {
		s.overlayKeys.Set(k)
	}
	s.dirtyKeys = append(s.dirtyKeys[:0], sp.dirtyKeys...)
	clear(s.readCache)
	clear(s.Aux)
	if sp.preCommitLen < len(s.preCommit) {
		s.preCommit = s.preCommit[:sp.preCommitLen]
	}
	s.mistrustDBAux = true
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
	overlayItr := s.overlayKeys.Iter()
	defer overlayItr.Release()
	overlayValid := false
	if s.overlayKeys.Len() > 0 {
		overlayValid = overlayItr.Seek(string(lower)) && overlayItr.Item() < upperStr
	}
	iter.First()
	for {
		backendValid := iter.Valid()
		if !backendValid && !overlayValid {
			break
		}
		// Decide which side to consume next.
		consumeOverlay := false
		if backendValid && overlayValid {
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
				overlayValid = false
			} else if overlayItr.Item() >= upperStr {
				overlayValid = false
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

