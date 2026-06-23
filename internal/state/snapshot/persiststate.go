package snapshot

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"hived-go/internal/state"
	"hived-go/internal/state/persiststore"
)

// persiststate is the "zero-snapshot node" container (chainbase-in-Go): the
// WHOLE non-cold state — the K/V mem rows + every MarshalSnapshot subsystem
// (accounts, the DGPO/feed/schedule singletons, witnesses, limit orders, …) —
// lives in ONE persiststore (mmap, append-only, index restored on open) instead
// of a snapshot dir of files + a manifest. Combined with a durable PersistCold
// tier (comments/votes, the bulk), the entire node state is two persiststore
// dirs: a restart opens both and resumes at head with NO snapshot file.
//
// It reuses each store's existing MarshalSnapshot/UnmarshalSnapshot (the same
// bytes a file snapshot would hold), so correctness is identical; only the
// container — and the persistence mechanism — is unified on persiststore.

const persistStateReserve = 32 << 30 // 32 GiB sparse (only touched pages cost disk)

// persiststate key prefixes (one byte) keep head / mem / per-subsystem rows
// distinct in the single store.
const (
	psHeadKey = "\x00head"
	psMemKey  = "\x01mem"
	psSubPfx  = "\x02" // + subsystem name
)

// SavePersist writes db's full non-cold state + the head block into a
// persiststore container under dir. Call on clean shutdown (alongside flushing
// the durable cold tier).
func SavePersist(db *state.Database, dir string, blockNum uint32) error {
	// Write a FRESH container to dir.tmp, then atomically swap it in. This (a)
	// makes the save atomic — a crash mid-write leaves dir.tmp (ignored) and the
	// prior dir intact, instead of a half-overwritten container that LoadPersist
	// would silently resume from; and (b) avoids dead-space accumulation — saving
	// into the existing append-only store each shutdown would append fresh copies
	// of all state and never reclaim the old (the persist-state dir would grow
	// unboundedly across restarts).
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("persiststate: clean tmp: %w", err)
	}
	if err := savePersistTo(db, tmp, blockNum); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("persiststate: clear old: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("persiststate: swap: %w", err)
	}
	return nil
}

// savePersistTo writes a complete, fresh persist-state container to dir. The
// head row is written LAST so that, even setting aside the atomic swap, a
// partially-written container lacks a head and is treated as absent on load.
func savePersistTo(db *state.Database, dir string, blockNum uint32) error {
	ps, err := persiststore.Open(dir, persistStateReserve)
	if err != nil {
		return fmt.Errorf("persiststate: open: %w", err)
	}
	defer ps.Close()

	// K/V mem rows (same framing as snapshot.writeMem, minus the file).
	var mem bytes.Buffer
	mem.WriteString("GHIVEMEM")
	var fhdr [4]byte
	binary.LittleEndian.PutUint32(fhdr[:], uint32(Format))
	mem.Write(fhdr[:])
	bw := bufio.NewWriter(&mem)
	if err := state.WriteAllRows(db, bw); err != nil {
		return fmt.Errorf("persiststate: write mem rows: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if err := ps.Set(psMemKey, mem.Bytes()); err != nil {
		return err
	}

	// Each MarshalSnapshot subsystem.
	for _, name := range db.SubsystemNames() {
		m, ok := db.GetSubsystem(name).(state.SnapshotMarshaler)
		if !ok {
			continue
		}
		var buf bytes.Buffer
		if err := m.MarshalSnapshot(&buf); err != nil {
			return fmt.Errorf("persiststate: marshal %s: %w", name, err)
		}
		if err := ps.Set(psSubPfx+name, buf.Bytes()); err != nil {
			return err
		}
	}

	// Head LAST — its presence marks a complete save.
	var hb [4]byte
	binary.BigEndian.PutUint32(hb[:], blockNum)
	if err := ps.Set(psHeadKey, hb[:]); err != nil {
		return err
	}
	return ps.Flush()
}

// PersistStateHead peeks the head block of a persist-state dir without loading
// (returns false if the dir is absent/empty/not a persist-state dir), so the
// caller can decide between resume and genesis + pick the HF era before opening
// the cold tier.
func PersistStateHead(dir string) (uint32, bool) {
	if _, err := os.Stat(filepath.Join(dir, "data.bin")); err != nil {
		return 0, false // no container yet → fresh (genesis) run
	}
	ps, err := persiststore.Open(dir, persistStateReserve)
	if err != nil {
		return 0, false
	}
	defer ps.Close()
	hb, ok := ps.Get(psHeadKey)
	if !ok || len(hb) < 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(hb), true
}

// LoadPersist restores what SavePersist wrote into db and returns the head
// block. Subsystems must already be registered (call EnsureXxx first), exactly
// like snapshot.Load. A durable cold tier must be attached BEFORE this call so
// the comment/vote stores' UnmarshalSnapshot can reconstruct their cold
// accounting.
func LoadPersist(db *state.Database, dir string) (uint32, error) {
	ps, err := persiststore.Open(dir, persistStateReserve)
	if err != nil {
		return 0, fmt.Errorf("persiststate: open: %w", err)
	}
	defer ps.Close()

	hb, ok := ps.Get(psHeadKey)
	if !ok || len(hb) < 4 {
		return 0, fmt.Errorf("persiststate: no head row in %s (not a persist-state dir)", dir)
	}
	head := binary.BigEndian.Uint32(hb)

	memBytes, ok := ps.Get(psMemKey)
	if !ok {
		return 0, fmt.Errorf("persiststate: no mem rows")
	}
	r := bytes.NewReader(memBytes)
	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return 0, fmt.Errorf("persiststate: read mem magic: %w", err)
	}
	if string(magic[:]) != "GHIVEMEM" {
		return 0, fmt.Errorf("persiststate: bad mem magic %q", magic)
	}
	var fhdr [4]byte
	if _, err := io.ReadFull(r, fhdr[:]); err != nil {
		return 0, err
	}
	if v := binary.LittleEndian.Uint32(fhdr[:]); v != uint32(Format) {
		return 0, fmt.Errorf("persiststate: mem format %d not supported", v)
	}
	if err := state.ReadAllRows(db, bufio.NewReader(r)); err != nil {
		return 0, fmt.Errorf("persiststate: read mem rows: %w", err)
	}

	for _, name := range db.SubsystemNames() {
		m, ok := db.GetSubsystem(name).(state.SnapshotMarshaler)
		if !ok {
			continue
		}
		blob, ok := ps.Get(psSubPfx + name)
		if !ok {
			continue // registered but absent from the container (never populated)
		}
		if err := m.UnmarshalSnapshot(bytes.NewReader(blob)); err != nil {
			return 0, fmt.Errorf("persiststate: unmarshal %s: %w", name, err)
		}
	}
	return head, nil
}
