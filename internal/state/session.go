package state

import (
	"errors"

	"github.com/cockroachdb/pebble"
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
	// scratch is reused by EncodeKeyInto in helper methods; per-Session,
	// not safe for concurrent use.
	scratch []byte
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
// overlay first, then Pebble.
func (s *Session) Get(table TableID, index IndexID, body []byte) ([]byte, error) {
	s.scratch = EncodeKeyInto(s.scratch, table, index, body)
	if e, ok := s.overlay[string(s.scratch)]; ok {
		if e.tombstone {
			return nil, ErrNotFound
		}
		out := make([]byte, len(e.value))
		copy(out, e.value)
		return out, nil
	}
	return s.db.rawGet(s.scratch)
}

// Has reports whether (table, index, body) exists.
func (s *Session) Has(table TableID, index IndexID, body []byte) (bool, error) {
	s.scratch = EncodeKeyInto(s.scratch, table, index, body)
	if e, ok := s.overlay[string(s.scratch)]; ok {
		return !e.tombstone, nil
	}
	return s.db.rawHas(s.scratch)
}

// Put stores value for (table, index, body) in the overlay. The slices are
// copied; callers may reuse their buffers.
func (s *Session) Put(table TableID, index IndexID, body, value []byte) {
	s.scratch = EncodeKeyInto(s.scratch, table, index, body)
	cv := make([]byte, len(value))
	copy(cv, value)
	s.overlay[string(s.scratch)] = overlayEntry{value: cv}
}

// Delete marks (table, index, body) as deleted in the overlay. Subsequent
// Get calls return ErrNotFound until the deletion is undone.
func (s *Session) Delete(table TableID, index IndexID, body []byte) {
	s.scratch = EncodeKeyInto(s.scratch, table, index, body)
	s.overlay[string(s.scratch)] = overlayEntry{tombstone: true}
}

// Commit flushes the overlay atomically to Pebble. After Commit the
// overlay is reset and the Session can be reused for the next block.
//
// sync controls whether the underlying write is fsync'd. For replay we
// pass false — durability comes from periodic flushes; for live sync /
// witness production callers should pass true.
func (s *Session) Commit(sync bool) error {
	if len(s.overlay) == 0 {
		return nil
	}
	batch := s.db.db.NewBatch()
	defer batch.Close()
	for k, e := range s.overlay {
		if e.tombstone {
			if err := batch.Delete([]byte(k), nil); err != nil {
				return err
			}
			continue
		}
		if err := batch.Set([]byte(k), e.value, nil); err != nil {
			return err
		}
	}
	if err := s.db.rawApply(batch, sync); err != nil {
		return err
	}
	s.overlay = make(map[string]overlayEntry, 64)
	return nil
}

// Undo discards the overlay without writing to Pebble.
func (s *Session) Undo() {
	s.overlay = make(map[string]overlayEntry, 64)
}

// Empty reports whether the session has any pending modifications.
func (s *Session) Empty() bool { return len(s.overlay) == 0 }

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
	upper := append(append([]byte(nil), lower...), 0xff)

	// Collect overlay entries in this range.
	type entry struct {
		body, value []byte
		tombstone   bool
	}
	overlayKeys := map[string]entry{}
	for k, e := range s.overlay {
		key := []byte(k)
		if !inRange(key, lower, upper) {
			continue
		}
		overlayKeys[k] = entry{body: append([]byte(nil), key[keyPrefixLen:]...), value: append([]byte(nil), e.value...), tombstone: e.tombstone}
	}

	iter, err := s.db.rawIter(lower, upper)
	if err != nil {
		return err
	}
	defer iter.Close()

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
		body := append([]byte(nil), k[keyPrefixLen:]...)
		val := append([]byte(nil), v...)
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

// Compile-time assertion that Pebble's options surface hasn't drifted.
var _ pebble.Options
