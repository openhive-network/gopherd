package state

import (
	"errors"
	"fmt"
	"io"
	"sort"
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
	// subsystems is the registry of typed in-memory containers that
	// chainbase-style domain code (objects.WitnessStore, …) registers
	// at construction. Each one implements SnapshotSubsystem so the
	// Session lifecycle (Snapshot/Restore/Commit/Undo) can roll their
	// undo-log positions in lock-step with the K/V overlay.
	subsystems []SnapshotSubsystem
	// subsystemReg holds the same subsystems indexed by a string name
	// so domain code can fetch them from a Session without an import
	// cycle. Lookup is cold (once per Session, then cached on the
	// caller side), so map overhead doesn't matter.
	subsystemReg map[string]any
	// UndoEnabled is the chain-wide flag toggled by Session.Snapshot
	// (true) and Session.DiscardSavepoint (false when depth reaches 0).
	// Subsystem stores (WitnessStore, AccountStore, …) consult it on
	// every Mutate/Put to decide whether to append an undo entry — the
	// per-block hooks (processProducerReward, cashout, schedule rebuild)
	// run with depth=0 and no rollback path, so the snap copy on every
	// Mutate is dead weight there. Default false; Snapshot enables.
	UndoEnabled bool
	// subPointsPool is a per-Database freelist of pre-allocated subsystem-
	// point capture slices (len(subsystems) capacity each). Session.Snapshot
	// borrows one per tx; DiscardSavepoint/Restore returns it. Eliminates the
	// per-tx make([]int, 0, 10) that contributed ~300 MB to a 4M-block run.
	subPointsPool [][]int
}

// SnapshotSubsystem is implemented by typed in-memory state containers
// (objects.WitnessStore, future Account/Comment stores). Each container
// owns its own undo log keyed by an integer "point"; the Session
// captures one point per subsystem in its Savepoint and rolls them
// back together on Restore. Commit drops accumulated undo entries.
//
// Snapshot semantics mirror chainbase's undo_session: every mutation
// writes a before-image into the log; Restore replays in reverse.
type SnapshotSubsystem interface {
	SnapshotPoint() int
	RestoreToPoint(int)
	CommitPoint()
}

// SnapshotMarshaler is OPTIONAL — implemented by typed subsystems that
// want their full primary state serialized into a state-snapshot file.
// The snapshot loader walks every registered subsystem and calls
// MarshalSnapshot on those that satisfy this interface; loaders mirror
// it via UnmarshalSnapshot. Format is private to each subsystem.
//
// Subsystems that DON'T implement this interface are skipped at
// snapshot time — useful for transient bookkeeping that can be
// re-derived from the K/V row plus other subsystem state.
type SnapshotMarshaler interface {
	MarshalSnapshot(w io.Writer) error
	UnmarshalSnapshot(r io.Reader) error
}

// RegisterSubsystem records a typed container plus its snapshot hooks.
// Returns the container slot so callers can fetch it back via
// GetSubsystem(name). Calling twice with the same name overwrites the
// registry entry but appends to the snapshot list — registrations
// happen once at Database construction so that's fine in practice.
func (d *Database) RegisterSubsystem(name string, sys any, snap SnapshotSubsystem) {
	if d.subsystemReg == nil {
		d.subsystemReg = make(map[string]any, 4)
	}
	d.subsystemReg[name] = sys
	if snap != nil {
		d.subsystems = append(d.subsystems, snap)
	}
}

// GetSubsystem returns the typed container registered under name, or
// nil if none was registered. Caller must type-assert.
func (d *Database) GetSubsystem(name string) any {
	if d.subsystemReg == nil {
		return nil
	}
	return d.subsystemReg[name]
}

// SubsystemNames returns every registered subsystem name in
// deterministic ascending order. Used by the snapshot saver to walk the
// registry without hardcoding the list.
func (d *Database) SubsystemNames() []string {
	if d.subsystemReg == nil {
		return nil
	}
	names := make([]string, 0, len(d.subsystemReg))
	for n := range d.subsystemReg {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// snapshotAll captures the current undo-log point of every registered
// subsystem.
func (d *Database) snapshotAll(out []int) []int {
	for _, s := range d.subsystems {
		out = append(out, s.SnapshotPoint())
	}
	return out
}

// borrowSubPoints returns a slice from the pool with cap ≥ len(subsystems).
// Caller must releaseSubPoints when done.
func (d *Database) borrowSubPoints() []int {
	if n := len(d.subPointsPool); n > 0 {
		buf := d.subPointsPool[n-1]
		d.subPointsPool = d.subPointsPool[:n-1]
		return buf[:0]
	}
	return make([]int, 0, len(d.subsystems))
}

// releaseSubPoints returns a slice to the pool.
func (d *Database) releaseSubPoints(buf []int) {
	if buf == nil || cap(buf) < len(d.subsystems) {
		return
	}
	d.subPointsPool = append(d.subPointsPool, buf[:0])
}

// restoreAll rolls every subsystem back to the captured points.
func (d *Database) restoreAll(points []int) {
	for i, s := range d.subsystems {
		if i >= len(points) {
			s.RestoreToPoint(0)
			continue
		}
		s.RestoreToPoint(points[i])
	}
}

// commitAll fires each subsystem's CommitPoint hook to release
// accumulated undo entries.
func (d *Database) commitAll() {
	for _, s := range d.subsystems {
		s.CommitPoint()
	}
}

// undoAll rolls every subsystem all the way back to its zero point —
// used by Session.Undo (full session discard).
func (d *Database) undoAll() {
	for _, s := range d.subsystems {
		s.RestoreToPoint(0)
	}
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

// rawScan invokes fn for every backend entry in [lower, upper) without
// materializing an iterator object. Used by Session.Range's no-overlay
// fast path.
func (d *Database) rawScan(lower, upper []byte, fn func(key, value []byte) (bool, error)) error {
	return d.backend.rawScan(lower, upper, fn)
}

// rawCommit applies the overlay to the backend atomically.
func (d *Database) rawCommit(overlay map[string]overlayEntry, sync bool) error {
	return d.backend.commit(overlay, sync)
}

// ErrNotFound is returned when a key has no value in either the session
// overlay or the underlying store.
var ErrNotFound = errors.New("state: not found")
