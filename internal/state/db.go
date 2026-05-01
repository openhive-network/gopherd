package state

import (
	"errors"
	"fmt"
)

// Database wraps a swappable storage backend with a small expiration-time
// hint cache used by per-block expirer hooks.
type Database struct {
	backend backend
	// expHints caches the minimum expiration time per (table, index) so
	// per-block expirer hooks can skip a Range when nothing is due.
	// Maintained by domain code via SetExpHint / NudgeExpHint.
	// Single-threaded replay only — no lock; add one if we ever go
	// concurrent.
	expHints map[uint32]uint32
	// Aux is a cross-Session typed-object cache for hot singletons
	// (FeedHistory, WitnessSchedule) that change rarely. Invalidated
	// by domain Put helpers when the underlying row mutates.
	Aux map[string]any
}

// AuxInvalidate drops a cached parsed object at the Database level.
func (d *Database) AuxInvalidate(key string) {
	if d.Aux != nil {
		delete(d.Aux, key)
	}
}

// Open opens a Pebble-backed Database at path. cacheSize is the byte
// size of the Pebble block cache (pass 0 for Pebble's default).
func Open(path string, cacheSize int64) (*Database, error) {
	b, err := openPebble(path, cacheSize)
	if err != nil {
		return nil, fmt.Errorf("state.Open %s: %w", path, err)
	}
	return &Database{backend: b, expHints: make(map[uint32]uint32, 16), Aux: make(map[string]any, 4)}, nil
}

// OpenMem opens a Database backed by an in-memory btree. State is
// volatile — intended for replay where we can re-derive from
// block_log on restart. Trades durability for ~3-5x throughput.
func OpenMem() *Database {
	return &Database{backend: openMem(), expHints: make(map[uint32]uint32, 16), Aux: make(map[string]any, 4)}
}

// expHintKey packs (table, index) into a single uint32 for the
// process-local expiration-hint map.
func expHintKey(table TableID, index IndexID) uint32 {
	return uint32(table)<<16 | uint32(index)
}

// GetExpHint returns the cached "earliest expiration time" for
// (table, index). The boolean is false when no hint has been recorded
// (e.g. fresh process, table never scanned). Callers MUST treat the
// no-hint case as "force the scan" rather than "nothing pending".
func (d *Database) GetExpHint(table TableID, index IndexID) (uint32, bool) {
	v, ok := d.expHints[expHintKey(table, index)]
	return v, ok
}

// SetExpHint records the earliest pending expiration time for
// (table, index). Called by per-block expirer hooks after a scan.
// Pass 0 to invalidate (force re-scan next block).
func (d *Database) SetExpHint(table TableID, index IndexID, time uint32) {
	if time == 0 {
		delete(d.expHints, expHintKey(table, index))
		return
	}
	d.expHints[expHintKey(table, index)] = time
}

// NudgeExpHint lowers the cached hint for (table, index) if newTime is
// earlier than the existing hint. Called by domain Put helpers when
// inserting a row with an expiration field, so the next-block hook
// won't erroneously skip a newly-inserted earlier expiration.
func (d *Database) NudgeExpHint(table TableID, index IndexID, newTime uint32) {
	k := expHintKey(table, index)
	if cur, ok := d.expHints[k]; ok && cur <= newTime {
		return
	}
	d.expHints[k] = newTime
}

// Close releases the database. After Close, all Sessions backed by this
// Database are invalid.
func (d *Database) Close() error {
	if d.backend == nil {
		return nil
	}
	err := d.backend.Close()
	d.backend = nil
	return err
}

// rawGet fetches the raw value for a fully-encoded physical key, returning
// (nil, ErrNotFound) when the key is absent.
func (d *Database) rawGet(physicalKey []byte) ([]byte, error) {
	return d.backend.rawGet(physicalKey)
}

// rawHas reports whether physicalKey exists.
func (d *Database) rawHas(physicalKey []byte) (bool, error) {
	return d.backend.rawHas(physicalKey)
}

// rawIter constructs an iterator over [lower, upper) on the underlying
// backend.
func (d *Database) rawIter(lower, upper []byte) (Iter, error) {
	return d.backend.rawIter(lower, upper)
}

// rawCommit applies the overlay to the backend atomically.
func (d *Database) rawCommit(overlay map[string]overlayEntry, sync bool) error {
	return d.backend.commit(overlay, sync)
}

// ErrNotFound is returned when a key has no value in either the session
// overlay or the underlying store.
var ErrNotFound = errors.New("state: not found")
