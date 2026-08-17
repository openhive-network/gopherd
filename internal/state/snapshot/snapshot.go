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
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hived-go/internal/config"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"

	"hived-go/internal/state"
)

// Format is bumped only on incompatible on-disk changes. Loaders refuse
// snapshots with a different format; we don't migrate.
const Format = 1

// HashAlgoCRC32C is the default checksum for newly written snapshots:
// hardware-accelerated Castagnoli CRC32 (~an order of magnitude faster than
// sha256 on multi-GB stores; these sums only guard against corruption, not
// adversaries). Legacy snapshots (empty hash_algo) keep verifying as sha256.
const HashAlgoCRC32C = "crc32c"

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func newSnapshotHash(algo string) hash.Hash {
	if algo == HashAlgoCRC32C {
		return crc32.New(castagnoli)
	}
	return sha256.New()
}

// Manifest is the JSON sidecar describing a snapshot directory.
type Manifest struct {
	Format      int    `json:"format"`
	BlockNum    uint32 `json:"block_num"`
	HeadBlockID string `json:"head_block_id,omitempty"` // hex; empty if unknown
	WrittenAtNs int64  `json:"written_at_ns"`
	// HashAlgo names the checksum in the *sha256 fields: "" = sha256
	// (legacy), "crc32c" = Castagnoli CRC32 (the write default).
	HashAlgo   string           `json:"hash_algo,omitempty"`
	MemFile    string           `json:"mem_file"`
	MemSHA256  string           `json:"mem_sha256"`
	Subsystems []SubsystemEntry `json:"subsystems"`
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
// Disk-full handling: a replay carries hours of un-recreatable state, so
// running out of disk mid-save must NOT abort the run. Save pre-flights the
// filesystem against an estimate (the newest sibling snapshot's size — the
// best predictor of this one's), and if the write still hits ENOSPC it
// removes the partial .tmp, logs loudly, and PAUSES (poll every 15s) until
// enough space is freed, then retries from scratch.
//
// Returns the final snapshot directory path.
func Save(db *state.Database, dir string, blockNum uint32, headBlockID string) (string, error) {
	for {
		path, err := saveOnce(db, dir, blockNum, headBlockID)
		if err == nil || !errors.Is(err, syscall.ENOSPC) {
			return path, err
		}
		// Drop the partial .tmp first — the operator shouldn't have to free
		// space for the failed attempt AND the retry.
		_ = os.RemoveAll(filepath.Join(dir, fmt.Sprintf("block-%d.tmp", blockNum)))
		need := snapshotSizeEstimate(dir)
		waitForDiskSpace(dir, need, err)
	}
}

// snapshotSizeEstimate returns the byte size of the newest block-* sibling
// under dir (0 if none) — the best available predictor of the next
// snapshot's size.
func snapshotSizeEstimate(dir string) uint64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	newest, best := uint64(0), ""
	for _, e := range entries {
		if !e.IsDir() || filepath.Ext(e.Name()) == ".tmp" {
			continue
		}
		var n uint64
		// Sscanf ignores trailing garbage, so require the exact round-trip
		// (otherwise a leftover "block-N.tmp" would be measured — and a
		// partial one under-estimates to near zero).
		if _, err := fmt.Sscanf(e.Name(), "block-%d", &n); err != nil || n == 0 || e.Name() != fmt.Sprintf("block-%d", n) {
			continue
		}
		if n > newest {
			newest, best = n, e.Name()
		}
	}
	if best == "" {
		return 0
	}
	var total uint64
	_ = filepath.WalkDir(filepath.Join(dir, best), func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total += uint64(fi.Size())
		}
		return nil
	})
	return total
}

// waitForDiskSpace blocks until the filesystem under dir has at least
// need*1.1 bytes free (or, when no estimate exists, until free space grew by
// 10GiB over the failure point), logging a loud actionable line each minute.
func waitForDiskSpace(dir string, need uint64, cause error) {
	free0, ok := diskFree(dir)
	if !ok {
		return // can't probe; let the retry fail again rather than spin blind
	}
	target := need + need/10
	if target == 0 {
		target = free0 + 10<<30
	}
	lastLog := time.Time{}
	for {
		free, ok := diskFree(dir)
		if !ok || free >= target {
			return
		}
		if time.Since(lastLog) >= time.Minute {
			fmt.Fprintf(os.Stderr,
				"snapshot: PAUSED on disk space at %s — %v; need ~%.1f GiB free, have %.1f GiB. Free space to resume (replay state is intact, retrying every 15s)\n",
				dir, cause, float64(target)/(1<<30), float64(free)/(1<<30))
			lastLog = time.Now()
		}
		time.Sleep(15 * time.Second)
	}
}

func saveOnce(db *state.Database, dir string, blockNum uint32, headBlockID string) (string, error) {
	if db == nil {
		return "", fmt.Errorf("snapshot: nil database")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("snapshot: mkdir parent: %w", err)
	}
	// Pre-flight: don't start a multi-GB write that predictably won't fit.
	if need := snapshotSizeEstimate(dir); need > 0 {
		if free, ok := diskFree(dir); ok && free < need+need/10 {
			waitForDiskSpace(dir, need, fmt.Errorf("pre-flight: snapshot needs ~%.1f GiB (newest sibling), only %.1f GiB free", float64(need)/(1<<30), float64(free)/(1<<30)))
		}
	}
	finalDir := filepath.Join(dir, fmt.Sprintf("block-%d", blockNum))
	tmpDir := finalDir + ".tmp"
	// Clean any leftover .tmp from a prior crashed run.
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(filepath.Join(tmpDir, "subsystems"), 0o755); err != nil {
		return "", fmt.Errorf("snapshot: mkdir tmp: %w", err)
	}

	// Write the K/V dump and every subsystem file CONCURRENTLY — the files
	// are independent, and the store marshalers only READ their (paused)
	// stores, so the small stores + mem.bin overlap with the giant
	// comment/vote streams instead of queueing behind them.
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	memPath := filepath.Join(tmpDir, "mem.bin")
	var memSHA string
	wg.Add(1)
	go func() {
		defer wg.Done()
		sum, err := writeMem(db, memPath)
		if err != nil {
			fail(fmt.Errorf("snapshot: write mem: %w", err))
			return
		}
		memSHA = sum
	}()
	subEntries := make([]SubsystemEntry, 0, 8)
	for _, name := range db.SubsystemNames() {
		sys := db.GetSubsystem(name)
		marshaler, ok := sys.(state.SnapshotMarshaler)
		if !ok {
			continue
		}
		name := name
		fname := name + ".bin"
		fpath := filepath.Join(tmpDir, "subsystems", fname)
		wg.Add(1)
		go func() {
			defer wg.Done()
			sum, err := writeSubsystem(marshaler, fpath, name)
			if err != nil {
				fail(fmt.Errorf("snapshot: write subsystem %s: %w", name, err))
				return
			}
			mu.Lock()
			subEntries = append(subEntries, SubsystemEntry{
				Name:   name,
				File:   filepath.Join("subsystems", fname),
				SHA256: sum,
			})
			mu.Unlock()
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return "", firstErr
	}
	sort.Slice(subEntries, func(i, j int) bool { return subEntries[i].Name < subEntries[j].Name })

	// Write the manifest last — it's the success marker.
	man := Manifest{
		Format:      Format,
		BlockNum:    blockNum,
		HeadBlockID: headBlockID,
		HashAlgo:    HashAlgoCRC32C,
		MemFile:     "mem.bin",
		MemSHA256:   memSHA,
		Subsystems:  subEntries,
	}
	if err := writeManifest(filepath.Join(tmpDir, "manifest.json"), &man); err != nil {
		return "", fmt.Errorf("snapshot: write manifest: %w", err)
	}

	// Atomic rename into place. A snapshot at block N is deterministic, so
	// an existing block-N dir (from an earlier run sharing this
	// snapshot-dir) is stale and safe to replace — os.Rename refuses to
	// overwrite a non-empty dir, which would otherwise abort a long replay
	// at the first periodic-snapshot collision. Remove any existing target
	// first (best-effort; a concurrent reader is not a supported mode).
	if _, statErr := os.Stat(finalDir); statErr == nil {
		_ = os.RemoveAll(finalDir)
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

	// Resolve every subsystem up front so registration errors surface before
	// any goroutine starts mutating stores.
	type subLoad struct {
		sub SubsystemEntry
		m   state.SnapshotMarshaler
	}
	// GHIVE_SNAPSHOT_SKIP_COMMENTS=1: leave comment_store/comment_vote_store
	// EMPTY (skip their multi-GB files). For feed/schedule-only diagnostics
	// with --tolerate-failing-ops: comment-touching ops fail and are skipped,
	// while witness state, feeds, and the schedule replay faithfully. NEVER
	// valid for consensus replay.
	skipComments := config.Env("GOPHERD_SNAPSHOT_SKIP_COMMENTS") == "1"
	loads := make([]subLoad, 0, len(man.Subsystems))
	for _, sub := range man.Subsystems {
		if skipComments && (sub.Name == "comment_store" || sub.Name == "comment_vote_store" || sub.Name == "comment_cashout_schedule_store") {
			fmt.Fprintf(os.Stderr, "snapshot: SKIPPING %s (GHIVE_SNAPSHOT_SKIP_COMMENTS)\n", sub.Name)
			continue
		}
		sys := db.GetSubsystem(sub.Name)
		if sys == nil {
			return nil, fmt.Errorf("snapshot: subsystem %q present in snapshot but not registered (call EnsureXxx before Load)", sub.Name)
		}
		marshaler, ok := sys.(state.SnapshotMarshaler)
		if !ok {
			return nil, fmt.Errorf("snapshot: subsystem %q does not implement SnapshotMarshaler", sub.Name)
		}
		loads = append(loads, subLoad{sub: sub, m: marshaler})
	}

	// Load the K/V dump and the subsystem files CONCURRENTLY: each file
	// fills its own independent store. The one shared resource is the cold
	// tier — comment_store and comment_vote_store both spill into it during
	// load and the persist backend is not safe for concurrent writers — so
	// those two run sequentially on ONE goroutine; everything else fans out.
	//
	// GHIVE_SNAPSHOT_LOAD_SERIAL=1 forces a fully sequential load: lower
	// peak memory/reclaim pressure (a 25GB parallel load on a 31GB box
	// repeatedly tripped systemd-oomd's ">50% pressure for >20s" kill) at
	// the cost of wall-clock.
	serialLoad := config.Env("GOPHERD_SNAPSHOT_LOAD_SERIAL") == "1"
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	runLoad := func(l subLoad) {
		fpath := filepath.Join(snapDir, l.sub.File)
		if err := loadSubsystem(l.m, fpath, l.sub.SHA256, man.HashAlgo, l.sub.Name); err != nil {
			fail(fmt.Errorf("snapshot: load subsystem %s: %w", l.sub.Name, err))
		}
	}
	if serialLoad {
		memPath := filepath.Join(snapDir, man.MemFile)
		if err := loadMem(db, memPath, man.MemSHA256, man.HashAlgo); err != nil {
			return nil, fmt.Errorf("snapshot: load mem: %w", err)
		}
		for _, l := range loads {
			if runLoad(l); firstErr != nil {
				return nil, firstErr
			}
		}
		return man, nil
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		memPath := filepath.Join(snapDir, man.MemFile)
		if err := loadMem(db, memPath, man.MemSHA256, man.HashAlgo); err != nil {
			fail(fmt.Errorf("snapshot: load mem: %w", err))
		}
	}()
	coldWriters := make([]subLoad, 0, 2)
	for _, l := range loads {
		if l.sub.Name == "comment_store" || l.sub.Name == "comment_vote_store" {
			coldWriters = append(coldWriters, l)
			continue
		}
		l := l
		wg.Add(1)
		go func() {
			defer wg.Done()
			runLoad(l)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, l := range coldWriters {
			runLoad(l)
		}
	}()
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	return man, nil
}

// countingWriter counts bytes so the per-file log line can report MB/s.
// When f is set it also drops the OS page cache for consumed ranges every
// dropCacheStride bytes — snapshot streams are strictly forward, and on a
// tight cgroup a multi-GB file otherwise evicts the heap + cold-tier pages.
type countingWriter struct {
	w       io.Writer
	f       *os.File
	n       int64
	advised int64
	// paceStart, when set, enables the GHIVE_SNAPSHOT_LOAD_PACE_MB rate
	// limit for this stream (loads only — saves must not be paced).
	paceStart time.Time
}

const dropCacheStride = 256 << 20

// snapshotZstd (GHIVE_SNAPSHOT_ZSTD=1) compresses every snapshot file with
// zstd on save (~3x smaller for the text-heavy comment corpus — the
// difference between a self-contained snapshot fitting a tight disk or
// not). Loads ALWAYS auto-detect by sniffing the zstd frame magic, so
// compressed and uncompressed snapshots interoperate transparently.
// Manifest checksums cover the FILE bytes (compressed when enabled).
var snapshotZstd = config.Env("GOPHERD_SNAPSHOT_ZSTD") == "1"

// zstdMagic is the 4-byte zstd frame header (RFC 8878). No snapshot
// subsystem format begins with these bytes, so sniffing is unambiguous.
var zstdMagic = []byte{0x28, 0xB5, 0x2F, 0xFD}

// MaybeCompressedReader wraps r in a buffered reader that transparently
// decompresses zstd snapshot files (sniffed by frame magic). For external
// tools (witnesssnap, vstrepair, …) that read snapshot subsystem files
// directly. The returned closer must be called when done.
func MaybeCompressedReader(r io.Reader) (*bufio.Reader, func(), error) {
	return maybeZstdReader(bufio.NewReaderSize(r, 1<<20))
}

// maybeZstdReader peeks br and, when it starts a zstd frame, returns a
// buffered decompressing reader; otherwise returns br unchanged. The
// returned closer must be closed (no-op for the passthrough case).
func maybeZstdReader(br *bufio.Reader) (*bufio.Reader, func(), error) {
	head, err := br.Peek(4)
	if err != nil || !bytes.Equal(head, zstdMagic) {
		return br, func() {}, nil // short/uncompressed files pass through
	}
	dec, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(2))
	if err != nil {
		return nil, nil, err
	}
	return bufio.NewReaderSize(dec, 1<<20), dec.Close, nil
}

// loadPaceMBs, when >0 (GHIVE_SNAPSHOT_LOAD_PACE_MB), caps the aggregate
// snapshot-load streaming rate. systemd-oomd kills any scope on a user box
// whose reclaim pressure stays >50% for 20s; an unthrottled 25GB load (read
// + cold-tier spill) reliably trips that on a 31GB machine. Pacing the read
// side paces the whole pipeline and keeps pressure under the trigger.
var loadPaceMBs = func() int64 {
	n, _ := strconv.ParseInt(config.Env("GOPHERD_SNAPSHOT_LOAD_PACE_MB"), 10, 64)
	return n
}()

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	if cw.f != nil && cw.n-cw.advised >= dropCacheStride {
		dropCacheUpTo(cw.f, cw.n)
		cw.advised = cw.n
	}
	if loadPaceMBs > 0 && cw.paceStart != (time.Time{}) {
		budget := time.Duration(cw.n/(loadPaceMBs<<20))*time.Second +
			time.Duration((cw.n%(loadPaceMBs<<20))*int64(time.Second)/(loadPaceMBs<<20))
		if ahead := budget - time.Since(cw.paceStart); ahead > 10*time.Millisecond {
			time.Sleep(ahead)
		}
	}
	return n, err
}

// logPhase prints one timing line per snapshot file that is big or slow
// enough to matter (the per-store breakdown is what tells us WHERE a slow
// save/load went — total-only timing hid a 2-orders-of-magnitude spread).
func logPhase(verb, name string, bytes int64, dur time.Duration) {
	if dur < 500*time.Millisecond && bytes < 64<<20 {
		return
	}
	mbps := float64(bytes) / (1 << 20) / dur.Seconds()
	fmt.Fprintf(os.Stderr, "snapshot: %s %s — %.1f MB in %.1fs (%.0f MB/s)\n",
		verb, name, float64(bytes)/(1<<20), dur.Seconds(), mbps)
}

// writeMem dumps every (key, value) in the K/V backend to path. Same
// stream format as state.Database.SaveSnapshot but without the
// embedded block-num header (the manifest carries it). Returns the
// hex checksum of the file.
func writeMem(db *state.Database, path string) (string, error) {
	start := time.Now()
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := newSnapshotHash(HashAlgoCRC32C)
	cw := &countingWriter{w: io.MultiWriter(f, h), f: f}
	var sink io.Writer = cw
	var encClose func() error
	if snapshotZstd {
		enc, zerr := zstd.NewWriter(cw, zstd.WithEncoderConcurrency(2))
		if zerr != nil {
			return "", zerr
		}
		sink, encClose = enc, enc.Close
	}
	w := bufio.NewWriterSize(sink, 1<<20)

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
	if encClose != nil {
		if err := encClose(); err != nil {
			return "", err
		}
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	logPhase("wrote", "mem.bin", cw.n, time.Since(start))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// loadMem reads a writeMem dump and re-inserts every (key, value) into
// the K/V backend. Verifies the checksum against expected.
func loadMem(db *state.Database, path, expectedSHA, hashAlgo string) error {
	start := time.Now()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := newSnapshotHash(hashAlgo)
	cr := &countingWriter{w: h, f: f, paceStart: time.Now()}
	raw := bufio.NewReaderSize(io.TeeReader(f, cr), 1<<20)
	r, decClose, err := maybeZstdReader(raw)
	if err != nil {
		return err
	}
	defer decClose()

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
	// Pull any compressed tail bytes through the tee so the hash covers
	// the whole file (the decoder may stop reading before raw EOF).
	if _, err := io.Copy(io.Discard, raw); err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if expectedSHA != "" && got != expectedSHA {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, expectedSHA)
	}
	logPhase("loaded", "mem.bin", cr.n, time.Since(start))
	return nil
}

// writeSubsystem invokes m.MarshalSnapshot and writes the result to
// path, returning the hex checksum.
func writeSubsystem(m state.SnapshotMarshaler, path, name string) (string, error) {
	start := time.Now()
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := newSnapshotHash(HashAlgoCRC32C)
	cw := &countingWriter{w: io.MultiWriter(f, h), f: f}
	var sink io.Writer = cw
	var encClose func() error
	if snapshotZstd {
		enc, zerr := zstd.NewWriter(cw, zstd.WithEncoderConcurrency(2))
		if zerr != nil {
			return "", zerr
		}
		sink, encClose = enc, enc.Close
	}
	bw := bufio.NewWriterSize(sink, 1<<20)
	if err := m.MarshalSnapshot(bw); err != nil {
		return "", err
	}
	if err := bw.Flush(); err != nil {
		return "", err
	}
	if encClose != nil {
		if err := encClose(); err != nil {
			return "", err
		}
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	logPhase("wrote", name, cw.n, time.Since(start))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// loadSubsystem opens path and feeds it into m.UnmarshalSnapshot.
// Verifies the checksum if expectedSHA is non-empty.
func loadSubsystem(m state.SnapshotMarshaler, path, expectedSHA, hashAlgo, name string) error {
	start := time.Now()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var h hash.Hash
	var cr *countingWriter
	var rawSrc io.Reader = f
	if expectedSHA != "" {
		h = newSnapshotHash(hashAlgo)
		cr = &countingWriter{w: h, f: f, paceStart: time.Now()}
		rawSrc = io.TeeReader(f, cr)
	}
	// The unmarshalers read record-at-a-time; without buffering every tiny
	// read goes through the TeeReader straight to a file read syscall.
	raw := bufio.NewReaderSize(rawSrc, 1<<20)
	src, decClose, err := maybeZstdReader(raw)
	if err != nil {
		return err
	}
	defer decClose()
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
		// And the raw (compressed) side, so the hash covers the whole file.
		if _, err := io.Copy(io.Discard, raw); err != nil {
			return fmt.Errorf("drain raw: %w", err)
		}
		got := hex.EncodeToString(h.Sum(nil))
		if got != expectedSHA {
			return fmt.Errorf("checksum mismatch: got %s, want %s", got, expectedSHA)
		}
		logPhase("loaded", name, cr.n, time.Since(start))
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
