// Package snapshot saves and loads the full ghive chain state as a
// directory of binary files plus a manifest. Designed for the debug
// loop: replay genesis once, snapshot every N blocks, then start
// future iterations from the nearest snapshot instead of re-replaying
// from block 1.
//
// Layout under <dir>/block-<N>/:
//
//	manifest.json                     -- format version, block number, sha256 list
//	mem.bin                           -- raw K/V dump (every (key, value) pair)
//	subsystems/<name>.bin             -- one file per registered subsystem
//	                                     (witness_store, account_store, …) that
//	                                     implements state.SnapshotMarshaler
//
// The K/V backend covers DGPO, FeedHistory, WitnessSchedule and every
// other state row that still goes through the generic K/V path; the
// per-subsystem files cover the typed in-memory containers (witness_store,
// account_store, comment_store, comment_vote_store, witness_vote_store)
// whose authoritative data is NOT in K/V.
//
// Atomicity: writes go to "block-<N>.tmp/" first; on success the
// directory is renamed to "block-<N>/". A half-written snapshot
// directory is therefore always recognizable by the .tmp suffix.
//
// This is a debug tool — no compression, no streaming, no migration.
package snapshot

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"

	"hived-go/internal/state"
)

// Format is bumped only on incompatible on-disk changes. Loaders refuse
// snapshots with a different format; we don't migrate.
const Format = 1

// Manifest is the JSON sidecar describing a snapshot directory.
type Manifest struct {
	Format       int               `json:"format"`
	BlockNum     uint32            `json:"block_num"`
	HeadBlockID  string            `json:"head_block_id,omitempty"` // hex; empty if unknown
	WrittenAtNs  int64             `json:"written_at_ns"`
	MemFile      string            `json:"mem_file"`
	MemSHA256    string            `json:"mem_sha256"`
	Subsystems   []SubsystemEntry  `json:"subsystems"`
}

// SubsystemEntry records one subsystem file in the manifest.
type SubsystemEntry struct {
	Name   string `json:"name"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

// Save serializes the full chain state of db into a new directory at
// <dir>/block-<blockNum>/. The directory must not already exist (we'll
// fail loudly rather than silently overwrite). headBlockID may be empty.
//
// Returns the final snapshot directory path.
func Save(db *state.Database, dir string, blockNum uint32, headBlockID string) (string, error) {
	if db == nil {
		return "", fmt.Errorf("snapshot: nil database")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("snapshot: mkdir parent: %w", err)
	}
	finalDir := filepath.Join(dir, fmt.Sprintf("block-%d", blockNum))
	tmpDir := finalDir + ".tmp"
	// Clean any leftover .tmp from a prior crashed run.
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(filepath.Join(tmpDir, "subsystems"), 0o755); err != nil {
		return "", fmt.Errorf("snapshot: mkdir tmp: %w", err)
	}

	// Write the K/V backend dump.
	memPath := filepath.Join(tmpDir, "mem.bin")
	memSHA, err := writeMem(db, memPath)
	if err != nil {
		return "", fmt.Errorf("snapshot: write mem: %w", err)
	}

	// Write each subsystem that supports MarshalSnapshot.
	subEntries := make([]SubsystemEntry, 0, 8)
	for _, name := range db.SubsystemNames() {
		sys := db.GetSubsystem(name)
		marshaler, ok := sys.(state.SnapshotMarshaler)
		if !ok {
			continue
		}
		fname := name + ".bin"
		fpath := filepath.Join(tmpDir, "subsystems", fname)
		sum, err := writeSubsystem(marshaler, fpath)
		if err != nil {
			return "", fmt.Errorf("snapshot: write subsystem %s: %w", name, err)
		}
		subEntries = append(subEntries, SubsystemEntry{
			Name:   name,
			File:   filepath.Join("subsystems", fname),
			SHA256: sum,
		})
	}
	sort.Slice(subEntries, func(i, j int) bool { return subEntries[i].Name < subEntries[j].Name })

	// Write the manifest last — it's the success marker.
	man := Manifest{
		Format:      Format,
		BlockNum:    blockNum,
		HeadBlockID: headBlockID,
		MemFile:     "mem.bin",
		MemSHA256:   memSHA,
		Subsystems:  subEntries,
	}
	if err := writeManifest(filepath.Join(tmpDir, "manifest.json"), &man); err != nil {
		return "", fmt.Errorf("snapshot: write manifest: %w", err)
	}

	// Atomic rename into place.
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return "", fmt.Errorf("snapshot: rename: %w", err)
	}
	return finalDir, nil
}

// Load reads a snapshot directory and restores its contents into db.
// Subsystems must already be registered on db before Load is called —
// the caller is responsible for invoking the appropriate EnsureXxx
// helpers (or otherwise calling RegisterSubsystem) so the loader has a
// destination for each per-subsystem file. Subsystems present in the
// snapshot but NOT registered cause Load to fail.
//
// Returns the manifest so callers can pick up at the right block.
func Load(db *state.Database, snapDir string) (*Manifest, error) {
	if db == nil {
		return nil, fmt.Errorf("snapshot: nil database")
	}
	man, err := readManifest(filepath.Join(snapDir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("snapshot: read manifest: %w", err)
	}
	if man.Format != Format {
		return nil, fmt.Errorf("snapshot: format %d not supported (want %d)", man.Format, Format)
	}

	// K/V dump.
	memPath := filepath.Join(snapDir, man.MemFile)
	if err := loadMem(db, memPath, man.MemSHA256); err != nil {
		return nil, fmt.Errorf("snapshot: load mem: %w", err)
	}

	// Per-subsystem files.
	for _, sub := range man.Subsystems {
		sys := db.GetSubsystem(sub.Name)
		if sys == nil {
			return nil, fmt.Errorf("snapshot: subsystem %q present in snapshot but not registered (call EnsureXxx before Load)", sub.Name)
		}
		marshaler, ok := sys.(state.SnapshotMarshaler)
		if !ok {
			return nil, fmt.Errorf("snapshot: subsystem %q does not implement SnapshotMarshaler", sub.Name)
		}
		fpath := filepath.Join(snapDir, sub.File)
		if err := loadSubsystem(marshaler, fpath, sub.SHA256); err != nil {
			return nil, fmt.Errorf("snapshot: load subsystem %s: %w", sub.Name, err)
		}
	}

	return man, nil
}

// writeMem dumps every (key, value) in the K/V backend to path. Same
// stream format as state.Database.SaveSnapshot but without the
// embedded block-num header (the manifest carries it). Returns the
// hex sha256 of the file.
func writeMem(db *state.Database, path string) (string, error) {
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	w := bufio.NewWriterSize(io.MultiWriter(f, h), 1<<20)

	// Header: magic + format version. No block number here — manifest
	// owns that.
	if _, err := w.Write([]byte{'G', 'H', 'I', 'V', 'E', 'M', 'E', 'M'}); err != nil {
		return "", err
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(Format))
	if _, err := w.Write(hdr[:]); err != nil {
		return "", err
	}

	if err := state.WriteAllRows(db, w); err != nil {
		return "", err
	}
	if err := w.Flush(); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// loadMem reads a writeMem dump and re-inserts every (key, value) into
// the K/V backend. Verifies sha256 against expected.
func loadMem(db *state.Database, path, expectedSHA string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	r := bufio.NewReaderSize(io.TeeReader(f, h), 1<<20)

	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return fmt.Errorf("read magic: %w", err)
	}
	if string(magic[:]) != "GHIVEMEM" {
		return fmt.Errorf("bad magic %q", magic)
	}
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[:])
	if ver != uint32(Format) {
		return fmt.Errorf("mem format %d not supported", ver)
	}
	if err := state.ReadAllRows(db, r); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if expectedSHA != "" && got != expectedSHA {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, expectedSHA)
	}
	return nil
}

// writeSubsystem invokes m.MarshalSnapshot and writes the result to
// path, returning the hex sha256.
func writeSubsystem(m state.SnapshotMarshaler, path string) (string, error) {
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	bw := bufio.NewWriterSize(io.MultiWriter(f, h), 1<<20)
	if err := m.MarshalSnapshot(bw); err != nil {
		return "", err
	}
	if err := bw.Flush(); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// loadSubsystem opens path and feeds it into m.UnmarshalSnapshot.
// Verifies sha256 if expectedSHA is non-empty.
func loadSubsystem(m state.SnapshotMarshaler, path, expectedSHA string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var h hash.Hash
	var src io.Reader = f
	if expectedSHA != "" {
		h = sha256.New()
		src = io.TeeReader(f, h)
	}
	if err := m.UnmarshalSnapshot(src); err != nil {
		return err
	}
	// Drain any tail bytes (the unmarshaler may stop before EOF if its
	// format records a count). We need to consume everything for the
	// hash, but only when checksum verification is requested.
	if h != nil {
		if _, err := io.Copy(io.Discard, src); err != nil {
			return fmt.Errorf("drain: %w", err)
		}
		got := hex.EncodeToString(h.Sum(nil))
		if got != expectedSHA {
			return fmt.Errorf("checksum mismatch: got %s, want %s", got, expectedSHA)
		}
	}
	return nil
}

func writeManifest(path string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
