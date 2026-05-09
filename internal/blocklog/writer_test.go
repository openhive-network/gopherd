package blocklog

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

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
