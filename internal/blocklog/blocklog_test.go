package blocklog

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"hived-go/internal/protocol/block"
)

// writeLegacyLog builds a synthetic single-file block_log + .index in dir
// from a slice of arbitrary block payloads. Returns the path to the main file.
//
// The "blocks" here are opaque bytes — for tests of Reader's offset math we
// don't need real FC-binary content. Reader exposes raw block bytes; parsing
// is layered on top via block.UnmarshalSignedHeader.
func writeLegacyLog(t *testing.T, dir string, blocks [][]byte) string {
	t.Helper()
	mainPath := filepath.Join(dir, legacyFileName)
	idxPath := mainPath + indexExtension

	var main bytes.Buffer
	var idx bytes.Buffer
	var trailer [trailerSize]byte
	off := uint64(0)
	for _, b := range blocks {
		// index entry: this block's start offset
		var e [indexEntrySize]byte
		binary.LittleEndian.PutUint64(e[:], off)
		idx.Write(e[:])

		// main: block bytes followed by 8-byte trailer = start offset
		main.Write(b)
		binary.LittleEndian.PutUint64(trailer[:], off)
		main.Write(trailer[:])
		off += uint64(len(b)) + trailerSize
	}

	if err := os.WriteFile(mainPath, main.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath, idx.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return mainPath
}

func TestReaderLegacyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	blocks := [][]byte{
		bytes.Repeat([]byte{0xa0}, 100),
		bytes.Repeat([]byte{0xa1}, 50),
		bytes.Repeat([]byte{0xa2}, 75),
	}
	mainPath := writeLegacyLog(t, dir, blocks)

	r, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if got := r.Head(); got != 3 {
		t.Fatalf("Head = %d, want 3", got)
	}
	for i, want := range blocks {
		got, err := r.Block(uint32(i + 1))
		if err != nil {
			t.Fatalf("Block(%d): %v", i+1, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("block %d: got %d bytes, want %d (first byte: got %x want %x)",
				i+1, len(got), len(want), got[0], want[0])
		}
	}
}

func TestReaderOutOfRange(t *testing.T) {
	dir := t.TempDir()
	mainPath := writeLegacyLog(t, dir, [][]byte{{1, 2, 3}})
	r, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if _, err := r.Block(0); err == nil {
		t.Error("Block(0) must error")
	}
	if _, err := r.Block(2); err == nil {
		t.Error("Block past head must error")
	}
}

// End-to-end: build a synthetic block_log containing real signed_block_header
// bytes and confirm Reader -> Unmarshal -> ConstructID forms a self-consistent
// chain (each block's `previous` equals the prior block's computed ID).
func TestEndToEndChainConsistency(t *testing.T) {
	dir := t.TempDir()

	// Build 5 chained blocks. Block N's previous == ID of block N-1; block 1's
	// previous is the all-zero ID (genesis convention).
	const n = 5
	headers := make([]block.SignedHeader, n)
	wireBlocks := make([][]byte, n)
	prev := block.ID{}
	for i := 0; i < n; i++ {
		hdr := block.SignedHeader{
			Header: block.Header{
				Previous:              prev,
				Timestamp:             1458835200 + uint32(i)*3,
				Witness:               "alice",
				TransactionMerkleRoot: [block.IDSize]byte{},
				Extensions:            []byte{0x00},
			},
		}
		// Distinct per-block signature so each ID differs.
		hdr.WitnessSignature[0] = byte(i + 1)
		headers[i] = hdr

		// Wire: signed_block_header || transactions(=empty: varint(0))
		w := block.MarshalSignedHeader(nil, &hdr)
		w = append(w, 0x00) // empty transactions vector
		wireBlocks[i] = w

		// Compute the ID from the signed-header byte range only.
		signedHdrLen := len(w) - 1 // strip the trailing transactions varint
		id := block.ConstructID(w[:signedHdrLen], uint32(i+1))
		prev = id
	}

	mainPath := writeLegacyLog(t, dir, wireBlocks)
	r, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if r.Head() != n {
		t.Fatalf("Head = %d, want %d", r.Head(), n)
	}

	var prevComputed block.ID
	for i := uint32(1); i <= n; i++ {
		raw, err := r.Block(i)
		if err != nil {
			t.Fatalf("Block(%d): %v", i, err)
		}
		hdr, signedLen, err := block.UnmarshalSignedHeader(raw)
		if err != nil {
			t.Fatalf("UnmarshalSignedHeader block %d: %v", i, err)
		}
		if i == 1 {
			if hdr.Previous != (block.ID{}) {
				t.Errorf("block 1 previous must be zero ID, got %x", hdr.Previous)
			}
		} else {
			if hdr.Previous != prevComputed {
				t.Errorf("block %d previous mismatch: got %x, want %x", i, hdr.Previous, prevComputed)
			}
		}
		id := block.ConstructID(raw[:signedLen], i)
		if id.Num() != i {
			t.Errorf("block %d ID embeds num=%d, want %d", i, id.Num(), i)
		}
		prevComputed = id
	}
}

// TestReaderTailContinuation exercises the "<main>.tail" continuation file:
// a ranged download of the canonical log's suffix whose trailers keep
// canonical (whole-log) offsets. The reader must stitch main+tail, apply the
// offset bias on every read, and rebuild the tail's index in memory.
func TestReaderTailContinuation(t *testing.T) {
	dir := t.TempDir()
	blocks := [][]byte{
		[]byte("block-one-payload"),
		[]byte("second-block"),
		[]byte("third-block-data-x"),
		[]byte("fourth"),
		[]byte("fifth-block-payload-zz"),
	}
	// Build the FULL canonical log in memory first.
	var full bytes.Buffer
	var offs []uint64
	off := uint64(0)
	for _, b := range blocks {
		offs = append(offs, off)
		full.Write(b)
		var tr [trailerSize]byte
		binary.LittleEndian.PutUint64(tr[:], off)
		full.Write(tr[:])
		off += uint64(len(b)) + trailerSize
	}
	// Split after block 3: main = canonical prefix, tail = canonical suffix.
	cut := offs[3] // start of block 4
	mainPath := filepath.Join(dir, legacyFileName)
	if err := os.WriteFile(mainPath, full.Bytes()[:cut], 0o644); err != nil {
		t.Fatal(err)
	}
	// Main has a sidecar index for its 3 blocks (tail has none — rebuilt).
	var idx bytes.Buffer
	for _, o := range offs[:3] {
		var e [indexEntrySize]byte
		binary.LittleEndian.PutUint64(e[:], o)
		idx.Write(e[:])
	}
	if err := os.WriteFile(mainPath+indexExtension, idx.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mainPath+".tail", full.Bytes()[cut:], 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.headBlock != 5 {
		t.Fatalf("head = %d, want 5", r.headBlock)
	}
	for i, want := range blocks {
		got, err := r.Block(uint32(i + 1))
		if err != nil {
			t.Fatalf("Block(%d): %v", i+1, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Block(%d) = %q, want %q", i+1, got, want)
		}
	}
	// The tail's rebuilt index must have been persisted as a sidecar.
	if _, err := os.Stat(mainPath + ".tail" + indexExtension); err != nil {
		t.Fatalf("tail index sidecar not persisted: %v", err)
	}
}
