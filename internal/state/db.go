package state

import (
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/tidwall/btree"
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

	// BatchSize controls how many Session.Commit calls are coalesced
	// into a single backend write. With BatchSize<=1 every Commit
	// flushes immediately (legacy behavior). With BatchSize>1 the
	// session overlay is moved into a Database-level batch overlay on
	// each Commit; the actual backend write fires only once we reach
	// BatchSize accumulated commits OR FlushBatch is invoked.
	//
	// Subsequent Sessions consult the batch overlay before falling
	// through to the backend, so reads remain consistent within the
	// process. Crash safety relies on the caller (chain.Database +
	// the block_log writer) being able to re-replay from the persisted
	// head on restart — see ghive-sync's recovery path.
	BatchSize int
	// batchOverlay holds writes from completed sessions that haven't
	// yet been flushed to backend. Mutated only under the chain's
	// applyMu write lock; safe to read under applyMu read lock from
	// other Session instances.
	batchOverlay map[string]overlayEntry
	// batchOverlayKeys is a sorted-by-key btree mirroring batchOverlay
	// so 3-way Range merges (session > batch > backend) advance both
	// cursors in lockstep without rescanning the whole map.
	batchOverlayKeys *btree.BTreeG[string]
	// pendingBlocks counts how many session.Commit calls have landed
	// into batchOverlay since the last backend flush. Used to fire
	// FlushBatch automatically at the BatchSize boundary.
	pendingBlocks int
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
	return newDatabase(b), nil
}

// OpenMem opens a Database backed by an in-memory btree. State is
// volatile — intended for replay where we can re-derive from
// block_log on restart. Trades durability for ~3-5x throughput.
func OpenMem() *Database {
	return newDatabase(openMem())
}

// newDatabase wraps a backend in a Database with the standard auxiliary
// maps populated. Callers can tune BatchSize after construction.
func newDatabase(b backend) *Database {
	return &Database{
		backend:          b,
		expHints:         make(map[uint32]uint32, 16),
		Aux:              make(map[string]any, 4),
		batchOverlay:     make(map[string]overlayEntry, 64),
		batchOverlayKeys: newOverlayKeyTree(),
	}
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
// Database are invalid. Any pending batched writes are flushed first;
// callers that wanted to discard them should call DiscardBatch beforehand.
func (d *Database) Close() error {
	if d.backend == nil {
		return nil
	}
	if err := d.FlushBatch(true); err != nil {
		return err
	}
	err := d.backend.Close()
	d.backend = nil
	return err
}

// SetBatchSize updates the commit-batch boundary. n<=1 disables
// batching (every Session.Commit flushes immediately). n>1 coalesces
// up to n consecutive Session.Commit calls into one backend write.
//
// If a partial batch is already pending it is flushed at the next
// Commit boundary that the new boundary covers; callers that need an
// immediate flush should follow up with FlushBatch(false).
func (d *Database) SetBatchSize(n int) {
	if n < 1 {
		n = 1
	}
	d.BatchSize = n
}

// FlushBatch writes every accumulated batched entry to the backend and
// resets the pending counter. Safe to call when the batch is empty.
// sync controls whether the underlying write fsyncs (Pebble only).
func (d *Database) FlushBatch(sync bool) error {
	if len(d.batchOverlay) == 0 {
		d.pendingBlocks = 0
		return nil
	}
	if err := d.backend.commit(d.batchOverlay, sync); err != nil {
		return err
	}
	clear(d.batchOverlay)
	d.batchOverlayKeys.Clear()
	d.pendingBlocks = 0
	return nil
}

// DiscardBatch drops every pending batched entry without writing them
// to the backend. Used by crash-recovery paths that intend to roll the
// in-memory state back to the persisted head and re-replay forward.
func (d *Database) DiscardBatch() {
	clear(d.batchOverlay)
	d.batchOverlayKeys.Clear()
	d.pendingBlocks = 0
}

// PendingBatch reports the number of session.Commit calls that have
// landed in the batch overlay since the last backend flush. Used by
// snapshot save / shutdown paths that need to force a flush.
func (d *Database) PendingBatch() int { return d.pendingBlocks }

// mergeSessionIntoBatch moves the contents of (overlay, dirtyKeys) into
// the Database batch overlay. Called by Session.Commit when batching is
// enabled. Returns the new pendingBlocks count after the merge.
//
// dirtyKeys may contain duplicates (Session.Put dedups consecutive
// writes only); the merge replays them in order so the final value
// wins. Tombstones are merged the same as Puts — they suppress the
// row at flush time even if a prior batched Put set a value.
func (d *Database) mergeSessionIntoBatch(overlay map[string]overlayEntry, dirtyKeys []string) int {
	for _, k := range dirtyKeys {
		e, ok := overlay[k]
		if !ok {
			continue
		}
		if _, present := d.batchOverlay[k]; !present {
			d.batchOverlayKeys.Set(k)
		}
		// Drop the snapshot-gen marker — once merged into the batch it
		// is never reused-in-place by another session, and a fresh
		// Session starts at gen 0 anyway.
		d.batchOverlay[k] = overlayEntry{value: e.value, tombstone: e.tombstone}
	}
	d.pendingBlocks++
	return d.pendingBlocks
}

// batchGet returns the batched entry for a fully-encoded key, if any.
// Returns the entry, true if found in the batch overlay; entry.tombstone
// distinguishes a pending delete from a pending value.
func (d *Database) batchGet(key string) (overlayEntry, bool) {
	if len(d.batchOverlay) == 0 {
		return overlayEntry{}, false
	}
	e, ok := d.batchOverlay[key]
	return e, ok
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
