package blocklog

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	bz "gopherd/internal/blocklog/zstd"
)

func zstdNewCompressor(t *testing.T) *bz.Compressor {
	t.Helper()
	return bz.NewCompressor()
}

func zstdBestDictNumberForBlock(n uint32) (uint8, bool) {
	return bz.BestDictNumberForBlock(n)
}

// TestWriterRoundTrip writes a few blocks via Writer, then opens the
// resulting file with Reader and asserts every block round-trips.
func TestWriterRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, legacyFileName)

	w, err := OpenForWrite(mainPath, false)
	if err != nil {
		t.Fatalf("OpenForWrite: %v", err)
	}

	blocks := [][]byte{
		bytes.Repeat([]byte{0xa0}, 100),
		bytes.Repeat([]byte{0xa1}, 50),
		bytes.Repeat([]byte{0xa2}, 75),
		bytes.Repeat([]byte{0xa3}, 1<<14), // larger to stress chunking
	}
	for i, b := range blocks {
		if err := w.Append(b); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if got, want := w.HeadBlock(), uint32(i+1); got != want {
			t.Fatalf("HeadBlock after append %d: got %d, want %d", i+1, got, want)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reader must report the right head, and reads must return the
	// exact bytes we wrote.
	r, err := Open(mainPath)
	if err != nil {
		t.Fatalf("Open after write: %v", err)
	}
	defer r.Close()

	if got, want := r.Head(), uint32(len(blocks)); got != want {
		t.Fatalf("Head: got %d, want %d", got, want)
	}
	for i, want := range blocks {
		got, err := r.Block(uint32(i + 1))
		if err != nil {
			t.Fatalf("Block(%d): %v", i+1, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("block %d: %d bytes, first=%x; want %d bytes, first=%x",
				i+1, len(got), got[0], len(want), want[0])
		}
	}
}

// TestWriterByteIdenticalToHand-rolled ensures the on-disk format matches
// the legacy hand-rolled writer used by the reader test fixture: identical
// main file and identical .index sidecar bytes.
func TestWriterMatchesLegacyFixture(t *testing.T) {
	dir := t.TempDir()
	blocks := [][]byte{
		{0x00, 0x01, 0x02, 0x03},
		{0x10, 0x11, 0x12, 0x13, 0x14, 0x15},
		bytes.Repeat([]byte{0x55}, 200),
	}

	// Reference: hand-rolled fixture from the existing reader test.
	refDir := t.TempDir()
	refMain := writeLegacyLog(t, refDir, blocks)
	refMainBytes, err := os.ReadFile(refMain)
	if err != nil {
		t.Fatal(err)
	}
	refIdxBytes, err := os.ReadFile(refMain + indexExtension)
	if err != nil {
		t.Fatal(err)
	}

	// Subject: writer-produced files.
	subjMain := filepath.Join(dir, legacyFileName)
	w, err := OpenForWrite(subjMain, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks {
		if err := w.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	subjMainBytes, err := os.ReadFile(subjMain)
	if err != nil {
		t.Fatal(err)
	}
	subjIdxBytes, err := os.ReadFile(subjMain + indexExtension)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(refMainBytes, subjMainBytes) {
		t.Errorf("main bytes differ: ref=%d bytes, subject=%d bytes", len(refMainBytes), len(subjMainBytes))
	}
	if !bytes.Equal(refIdxBytes, subjIdxBytes) {
		t.Errorf("index bytes differ: ref=%d bytes, subject=%d bytes", len(refIdxBytes), len(subjIdxBytes))
	}
}

// TestWriterAppendRawWithAttributes checks the trailer flag bits round-trip
// for a compressed-with-dict entry. We don't actually compress here — the
// writer is opaque to the bytes — but we verify the trailer encoding exactly
// matches hived's combine_block_start_pos_with_flags.
func TestWriterAppendRawWithAttributes(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, legacyFileName)

	w, err := OpenForWrite(mainPath, false)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte{0xff}, 32)
	attr := Attributes{Compressed: true, HasDictNum: true, DictionaryNum: 42}
	if err := w.AppendRaw(body, attr); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// The trailer is the last 8 bytes of the file.
	raw, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != len(body)+trailerSize {
		t.Fatalf("file size = %d, want %d", len(raw), len(body)+trailerSize)
	}
	packed := binary.LittleEndian.Uint64(raw[len(body):])
	off, gotAttr := decodeTrailer(packed)
	if off != 0 {
		t.Errorf("offset: got %d, want 0", off)
	}
	if !gotAttr.Compressed {
		t.Error("compressed flag lost")
	}
	if !gotAttr.HasDictNum || gotAttr.DictionaryNum != 42 {
		t.Errorf("dict: got HasDictNum=%v DictionaryNum=%d, want true,42",
			gotAttr.HasDictNum, gotAttr.DictionaryNum)
	}
}

// TestWriterCompressedRoundTrip writes a few blocks via the compressing
// writer and asserts each one round-trips through Reader.Block (which
// transparently decompresses).
func TestWriterCompressedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, legacyFileName)

	w, err := OpenForWrite(mainPath, true)
	if err != nil {
		t.Fatalf("OpenForWrite(compress=true): %v", err)
	}
	blocks := [][]byte{
		bytes.Repeat([]byte{0xa0}, 200),
		bytes.Repeat([]byte("hive"), 200),
		bytes.Repeat([]byte{0xff}, 1<<14),
	}
	for i, b := range blocks {
		if err := w.Append(b); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Each block on disk should now be smaller than its raw form (the input
	// is repetitive — compresses well).
	rawTotal := 0
	for _, b := range blocks {
		rawTotal += len(b)
	}
	stat, err := os.Stat(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	if int(stat.Size()) >= rawTotal+len(blocks)*trailerSize {
		t.Errorf("file size %d not smaller than uncompressed %d (compression ineffective?)",
			stat.Size(), rawTotal+len(blocks)*trailerSize)
	}

	r, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Head() != uint32(len(blocks)) {
		t.Errorf("Head = %d, want %d", r.Head(), len(blocks))
	}
	for i, want := range blocks {
		got, err := r.Block(uint32(i + 1))
		if err != nil {
			t.Fatalf("Block(%d): %v", i+1, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("block %d: %d bytes mismatch", i+1, len(got))
		}
		// Trailer should advertise compressed=true with the dict for
		// block_num/1M (i.e. 0 for blocks 1..3).
		attr, err := r.Attributes(uint32(i + 1))
		if err != nil {
			t.Fatal(err)
		}
		if !attr.Compressed {
			t.Errorf("block %d: compressed=false (want true)", i+1)
		}
		if !attr.HasDictNum || attr.DictionaryNum != 0 {
			t.Errorf("block %d: dict=%v/%d, want true/0", i+1, attr.HasDictNum, attr.DictionaryNum)
		}
	}
}

// TestWriterByteIdenticalToMainnet picks the first 100 blocks of the local
// mainnet block_log, decompresses them via Reader, then re-compresses them
// with our compressing writer. The resulting file's main-bytes must be
// byte-identical to the corresponding slice of the source file.
//
// Skipped when ./block_log isn't present (CI runs without the data file).
func TestWriterByteIdenticalToMainnet(t *testing.T) {
	src := findMainnetBlockLogForTest()
	if src == "" {
		t.Skip("./block_log not available; skipping mainnet byte-identical test")
	}

	const nBlocks = 100
	srcReader, err := Open(src)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	if srcReader.Head() < nBlocks {
		srcReader.Close()
		t.Skipf("source only has %d blocks", srcReader.Head())
	}

	// Pull decompressed bytes for each block.
	decoded := make([][]byte, nBlocks)
	for i := uint32(1); i <= nBlocks; i++ {
		b, err := srcReader.Block(i)
		if err != nil {
			srcReader.Close()
			t.Fatalf("source Block(%d): %v", i, err)
		}
		decoded[i-1] = b
	}
	srcReader.Close()

	// Re-compress with our writer.
	dir := t.TempDir()
	subjPath := filepath.Join(dir, legacyFileName)
	w, err := OpenForWrite(subjPath, true)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range decoded {
		if err := w.Append(b); err != nil {
			t.Fatalf("Append(%d): %v", i+1, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Read the source file's matching prefix slice (everything from offset 0
	// up to the end of block N's trailer).
	srcMainSize := mustEndOfBlockOffset(t, src, nBlocks)
	srcSlice := make([]byte, srcMainSize)
	sf, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sf.ReadAt(srcSlice, 0); err != nil {
		sf.Close()
		t.Fatal(err)
	}
	sf.Close()

	gotBytes, err := os.ReadFile(subjPath)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(gotBytes, srcSlice) {
		// Find first divergence to aid debugging.
		minLen := len(gotBytes)
		if len(srcSlice) < minLen {
			minLen = len(srcSlice)
		}
		first := -1
		for i := 0; i < minLen; i++ {
			if gotBytes[i] != srcSlice[i] {
				first = i
				break
			}
		}
		t.Fatalf("byte-identical mismatch: got %d bytes, source %d bytes; first diff at offset %d",
			len(gotBytes), len(srcSlice), first)
	}
}

// findMainnetBlockLogForTest mirrors the lookup in cmd/gopherd's test
// helper but lives here to avoid importing main.
func findMainnetBlockLogForTest() string {
	candidates := []string{
		"../../block_log",
		"../block_log",
		"./block_log",
		"/home/martin/projects/hived-go/block_log",
	}
	for _, p := range candidates {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	return ""
}

// mustEndOfBlockOffset returns the file offset of the byte immediately after
// blockN's 8-byte trailer (i.e. the size of the prefix that covers blocks
// 1..blockN). Used to slice the source file for byte-identical comparisons.
func mustEndOfBlockOffset(t *testing.T, path string, blockN uint32) int {
	t.Helper()
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, end, err := r.BlockExtent(blockN)
	if err != nil {
		t.Fatal(err)
	}
	return int(end + 8)
}

// TestWriterByteIdenticalAcrossDictBoundary checks that the per-block dict
// selection rolls over correctly at block 1_000_000 (dict 0 → dict 1) by
// re-compressing a small window straddling the boundary.
func TestWriterByteIdenticalAcrossDictBoundary(t *testing.T) {
	src := findMainnetBlockLogForTest()
	if src == "" {
		t.Skip("./block_log not available")
	}
	r, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Head() < 1_000_005 {
		t.Skipf("source only has %d blocks, need >= 1_000_005", r.Head())
	}

	// We can't have the writer start at block 999_998: it always starts at 1.
	// Instead, exercise the dict-selection logic in isolation by compressing
	// the decoded blocks via zstd.NewCompressor and comparing each one's
	// bytes individually.
	const startBlock = 999_998
	const endBlock = 1_000_005
	c := zstdNewCompressor(t)

	for n := uint32(startBlock); n <= endBlock; n++ {
		decoded, err := r.Block(n)
		if err != nil {
			t.Fatalf("Block(%d): %v", n, err)
		}
		raw, attr, err := r.RawBlockBytes(n)
		if err != nil {
			t.Fatalf("RawBlockBytes(%d): %v", n, err)
		}
		if !attr.Compressed || !attr.HasDictNum {
			t.Fatalf("block %d not compressed-with-dict", n)
		}
		our, err := c.Compress(decoded, attr.DictionaryNum)
		if err != nil {
			t.Fatalf("compress(%d, dict %d): %v", n, attr.DictionaryNum, err)
		}
		if !bytes.Equal(our, raw) {
			t.Errorf("block %d: re-compressed bytes differ from disk (got %d, disk %d)",
				n, len(our), len(raw))
		}
		// Sanity: the dict number should match BestDictNumberForBlock.
		want, _ := zstdBestDictNumberForBlock(n)
		if attr.DictionaryNum != want {
			t.Errorf("block %d: disk uses dict %d, BestDictNumberForBlock says %d", n, attr.DictionaryNum, want)
		}
	}
}

// TestWriterReopenContinues asserts an existing file is appended to,
// not overwritten.
func TestWriterReopenContinues(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, legacyFileName)

	w, err := OpenForWrite(mainPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append([]byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatal(err)
	}
	if err := w.Append([]byte{0x10, 0x11, 0x12, 0x13}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	w, err = OpenForWrite(mainPath, false)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := w.HeadBlock(); got != 2 {
		t.Errorf("HeadBlock after reopen: got %d, want 2", got)
	}
	if err := w.Append([]byte{0x20, 0x21}); err != nil {
		t.Fatal(err)
	}
	if got := w.HeadBlock(); got != 3 {
		t.Errorf("HeadBlock after one more: got %d, want 3", got)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Head() != 3 {
		t.Errorf("Reader Head: got %d, want 3", r.Head())
	}
	for i, want := range [][]byte{{0x01, 0x02, 0x03}, {0x10, 0x11, 0x12, 0x13}, {0x20, 0x21}} {
		got, err := r.Block(uint32(i + 1))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("block %d: got %x, want %x", i+1, got, want)
		}
	}
}
