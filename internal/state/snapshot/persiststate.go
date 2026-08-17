package snapshot

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gopherd/internal/state"
	"gopherd/internal/state/persiststore"
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
	// psChunkPfx + <key> + "\x00" + <4-byte BE index> holds one bounded slice of
	// a large value. See chunkWriter: values are written in pieces so a save
	// never materialises the whole serialized state in RAM.
	psChunkPfx = "\x03"
	// psChunkCntPfx + <key> holds the 4-byte BE chunk count for a chunked value.
	psChunkCntPfx = "\x04"
)

// psChunkSize bounds how much of a value is held in memory at once during a
// save. The previous code buffered an ENTIRE value — all K/V rows in one
// bytes.Buffer, then each subsystem in another — which is fine on a small chain
// and fatal on a full one: at mainnet scale that is multiple GB on top of the
// live heap, and it OOM-killed a node during shutdown (2026-08-10), losing both
// the container and the shutdown snapshot that would have run after it.
//
// 64 MiB keeps peak save overhead flat regardless of chain size.
const psChunkSize = 64 << 20

// chunkWriter is an io.Writer that flushes into persiststore in psChunkSize
// pieces under key/0, key/1, … and finally records the count, so a value of any
// size can be written with bounded memory.
type chunkWriter struct {
	ps    *persiststore.Store
	key   string
	buf   []byte
	count uint32
	err   error
}

func newChunkWriter(ps *persiststore.Store, key string) *chunkWriter {
	return &chunkWriter{ps: ps, key: key, buf: make([]byte, 0, psChunkSize)}
}

func chunkKey(key string, i uint32) string {
	var ib [4]byte
	binary.BigEndian.PutUint32(ib[:], i)
	return psChunkPfx + key + "\x00" + string(ib[:])
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n := len(p)
	for len(p) > 0 {
		space := psChunkSize - len(w.buf)
		take := len(p)
		if take > space {
			take = space
		}
		w.buf = append(w.buf, p[:take]...)
		p = p[take:]
		if len(w.buf) == psChunkSize {
			if err := w.flush(); err != nil {
				w.err = err
				return 0, err
			}
		}
	}
	return n, nil
}

func (w *chunkWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	if err := w.ps.Set(chunkKey(w.key, w.count), w.buf); err != nil {
		return err
	}
	w.count++
	w.buf = w.buf[:0]
	return nil
}

// Close flushes the tail and records the chunk count, which is what marks the
// value complete — a partially written value has no count and reads as absent.
func (w *chunkWriter) Close() error {
	if w.err != nil {
		return w.err
	}
	if err := w.flush(); err != nil {
		return err
	}
	var cb [4]byte
	binary.BigEndian.PutUint32(cb[:], w.count)
	return w.ps.Set(psChunkCntPfx+w.key, cb[:])
}

// chunkReader returns an io.Reader over a chunked value, fetching one chunk at
// a time. persiststore.Get hands back a view into the mmap, so this does not
// copy the whole value into the heap. Reports false when the key was not
// written in chunked form (older container, or an interrupted save).
func chunkReader(ps *persiststore.Store, key string) (io.Reader, bool) {
	cb, ok := ps.Get(psChunkCntPfx + key)
	if !ok || len(cb) < 4 {
		return nil, false
	}
	n := binary.BigEndian.Uint32(cb)
	readers := make([]io.Reader, 0, n)
	for i := uint32(0); i < n; i++ {
		blob, ok := ps.Get(chunkKey(key, i))
		if !ok {
			return nil, false
		}
		readers = append(readers, bytes.NewReader(blob))
	}
	return io.MultiReader(readers...), true
}

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

	// K/V mem rows (same framing as snapshot.writeMem, minus the file), streamed
	// in bounded chunks rather than buffered whole — see psChunkSize.
	memW := newChunkWriter(ps, psMemKey)
	bw := bufio.NewWriter(memW)
	if _, err := bw.WriteString("GHIVEMEM"); err != nil {
		return err
	}
	var fhdr [4]byte
	binary.LittleEndian.PutUint32(fhdr[:], uint32(Format))
	if _, err := bw.Write(fhdr[:]); err != nil {
		return err
	}
	if err := state.WriteAllRows(db, bw); err != nil {
		return fmt.Errorf("persiststate: write mem rows: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if err := memW.Close(); err != nil {
		return fmt.Errorf("persiststate: write mem chunks: %w", err)
	}

	// Each MarshalSnapshot subsystem.
	for _, name := range db.SubsystemNames() {
		m, ok := db.GetSubsystem(name).(state.SnapshotMarshaler)
		if !ok {
			continue
		}
		// Chunked for the same reason as the mem rows: comment_store alone is
		// multiple GB on a full chain, and buffering it whole is what killed the
		// process.
		subW := newChunkWriter(ps, psSubPfx+name)
		sbw := bufio.NewWriter(subW)
		if err := m.MarshalSnapshot(sbw); err != nil {
			return fmt.Errorf("persiststate: marshal %s: %w", name, err)
		}
		if err := sbw.Flush(); err != nil {
			return err
		}
		if err := subW.Close(); err != nil {
			return fmt.Errorf("persiststate: write %s chunks: %w", name, err)
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

	// Chunked form first; fall back to the original single-blob layout so a
	// container written by an older build still loads.
	var r io.Reader
	if cr, ok := chunkReader(ps, psMemKey); ok {
		r = cr
	} else if memBytes, ok := ps.Get(psMemKey); ok {
		r = bytes.NewReader(memBytes)
	} else {
		return 0, fmt.Errorf("persiststate: no mem rows")
	}
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
		var sr io.Reader
		if cr, ok := chunkReader(ps, psSubPfx+name); ok {
			sr = cr
		} else if blob, ok := ps.Get(psSubPfx + name); ok {
			sr = bytes.NewReader(blob) // pre-chunking container
		} else {
			continue // registered but absent from the container (never populated)
		}
		// Per-subsystem timing, mirroring the snapshot loader's output. Without
		// it a slow resume is unattributable: on 2026-08-10 a 22-minute
		// persist-state load was silent the whole way, and the cause (one
		// subsystem re-spilling its cold rows because the cold backend was not
		// durable) was only found by inference from the SNAPSHOT loader's
		// per-subsystem rates. Cheap — one line per subsystem, once per start.
		start := time.Now()
		if err := m.UnmarshalSnapshot(sr); err != nil {
			return 0, fmt.Errorf("persiststate: unmarshal %s: %w", name, err)
		}
		if el := time.Since(start); el > 200*time.Millisecond {
			fmt.Fprintf(os.Stderr, "persist-state: loaded %s in %.1fs\n", name, el.Seconds())
		}
	}
	return head, nil
}
