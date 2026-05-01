package state

import (
	"errors"
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
	// touched flags which (table, index) pairs have been written via
	// Put/Delete in this Session. Range consults this to skip the
	// overlay walk entirely when no overlay write has hit the table —
	// the common case for read-only per-block expirer hooks scanning a
	// by-expiration index that the current block hasn't mutated.
	touched map[uint32]bool
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
	return s.touched[uint32(table)<<16|uint32(index)]
}

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
		db:      db,
		overlay: make(map[string]overlayEntry, 64),
	}
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
	cv := make([]byte, len(value))
	copy(cv, value)
	fullKey := string(s.scratch)
	s.overlay[fullKey] = overlayEntry{value: cv}
	s.dirtyKeys = append(s.dirtyKeys, fullKey)
	if s.readCache != nil {
		delete(s.readCache, fullKey)
	}
	s.markTouched(table, index)
}

// Delete marks (table, index, body) as deleted in the overlay. Subsequent
// Get calls return ErrNotFound until the deletion is undone.
func (s *Session) Delete(table TableID, index IndexID, body []byte) {
	s.scratch = EncodeKeyInto(s.scratch, table, index, body)
	fullKey := string(s.scratch)
	s.overlay[fullKey] = overlayEntry{tombstone: true}
	s.dirtyKeys = append(s.dirtyKeys, fullKey)
	if s.readCache != nil {
		delete(s.readCache, fullKey)
	}
	s.markTouched(table, index)
}

func (s *Session) markTouched(table TableID, index IndexID) {
	if s.touched == nil {
		s.touched = make(map[uint32]bool, 8)
	}
	s.touched[uint32(table)<<16|uint32(index)] = true
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
		clear(s.touched)
		s.dirtyKeys = s.dirtyKeys[:0]
		return nil
	}
	if err := s.db.backend.commitDirty(s.overlay, s.dirtyKeys, sync); err != nil {
		return err
	}
	clear(s.overlay)
	clear(s.readCache)
	clear(s.Aux)
	clear(s.touched)
	s.dirtyKeys = s.dirtyKeys[:0]
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
	clear(s.touched)
	s.preCommit = s.preCommit[:0]
	s.dirtyKeys = s.dirtyKeys[:0]
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
	overlay   map[string]overlayEntry
	touched   map[uint32]bool
	dirtyKeys []string
}

// Snapshot captures the current overlay state. Subsequent Put/Delete
// calls modify the live overlay; Restore(sp) reverts to the snapshot.
//
// Cost: O(len(overlay)) — a shallow copy of the map. The values slice
// is shared, since Put always allocates a fresh slice.
func (s *Session) Snapshot() Savepoint {
	cp := make(map[string]overlayEntry, len(s.overlay))
	for k, v := range s.overlay {
		cp[k] = v
	}
	t := make(map[uint32]bool, len(s.touched))
	for k, v := range s.touched {
		t[k] = v
	}
	dk := append([]string(nil), s.dirtyKeys...)
	return Savepoint{overlay: cp, touched: t, dirtyKeys: dk}
}

// Restore reverts the overlay to the state captured by sp. Any
// Put/Delete since the snapshot is discarded.
func (s *Session) Restore(sp Savepoint) {
	cp := make(map[string]overlayEntry, len(sp.overlay))
	for k, v := range sp.overlay {
		cp[k] = v
	}
	s.overlay = cp
	t := make(map[uint32]bool, len(sp.touched))
	for k, v := range sp.touched {
		t[k] = v
	}
	s.touched = t
	s.dirtyKeys = append(s.dirtyKeys[:0], sp.dirtyKeys...)
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

	// Collect overlay entries in this range. Skip the entire walk when
	// no Put/Delete in this Session has hit (table, index) — the common
	// case for read-only per-block expirer hooks. Entry bodies alias
	// the overlay map keys (stable for the Session lifetime), and
	// values alias the overlay value slices (allocated once at Put);
	// no copy needed.
	type entry struct {
		body, value []byte
		tombstone   bool
	}
	var overlayKeys map[string]entry
	if s.touched[uint32(table)<<16|uint32(index)] {
		for k, e := range s.overlay {
			key := []byte(k)
			if !inRange(key, lower, upper) {
				continue
			}
			if overlayKeys == nil {
				overlayKeys = make(map[string]entry, 4)
			}
			overlayKeys[k] = entry{body: key[keyPrefixLen:], value: e.value, tombstone: e.tombstone}
		}
	}

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

	// Range emits backend slices directly to fn — callers must not
	// retain or mutate them past the callback return (mem backend
	// aliases btree storage, Pebble aliases iterator scratch). Match
	// the same read-only contract as Session.Get.
	for iter.First(); iter.Valid(); iter.Next() {
		k := iter.Key()
		if e, ok := overlayKeys[string(k)]; ok {
			delete(overlayKeys, string(k))
			if e.tombstone {
				continue
			}
			if err := emit(e.body, e.value); err != nil {
				if errors.Is(err, errStopIteration) {
					return nil
				}
				return err
			}
			continue
		}
		v, err := iter.ValueAndErr()
		if err != nil {
			return err
		}
		body := k[keyPrefixLen:]
		val := v
		if err := emit(body, val); err != nil {
			if errors.Is(err, errStopIteration) {
				return nil
			}
			return err
		}
	}
	if err := iter.Error(); err != nil {
		return err
	}

	// Emit overlay-only entries (not seen during Pebble scan).
	for _, e := range overlayKeys {
		if e.tombstone {
			continue
		}
		if err := emit(e.body, e.value); err != nil {
			if errors.Is(err, errStopIteration) {
				return nil
			}
			return err
		}
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

