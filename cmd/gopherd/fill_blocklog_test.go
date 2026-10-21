package main

import (
	"path/filepath"
	"testing"

	"gopherd/internal/blocklog"
	"gopherd/internal/protocol/block"
)

// TestFillBlocklogGap: a block_log missing block 3 (block 4 stored in its
// slot) shows one gap at position 3 naming block 3, and the rewrite puts
// block 3 back so the log holds blocks 1..6 in place again.
func TestFillBlocklogGap(t *testing.T) {
	var blocks [][]byte
	var prev block.ID
	for n := uint32(1); n <= 6; n++ {
		sh := block.SignedHeader{Header: block.Header{Previous: prev, Timestamp: 3 * n, Witness: "initminer", Extensions: []byte{0}}}
		hdr := block.MarshalSignedHeader(nil, &sh)
		prev = block.ConstructID(hdr, n)
		blocks = append(blocks, append(hdr, 0)) // no transactions
	}
	path := filepath.Join(t.TempDir(), "block_log")
	w, err := blocklog.OpenForWrite(path, true)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range blocks {
		if i == 2 {
			continue // block 3 never written
		}
		if err := w.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()

	r, err := blocklog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	gaps, err := findBlocklogGaps(r, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 || gaps[0].pos != 3 || gaps[0].want.Num() != 3 || gaps[0].after.Num() != 2 {
		t.Fatalf("gaps = %+v, want one before position 3 naming block 3", gaps)
	}
	if err := rewriteBlocklogTail(path, r, gaps, map[uint32][][]byte{3: {blocks[2]}}); err != nil {
		t.Fatal(err)
	}
	r2, err := blocklog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if r2.Head() != 6 {
		t.Fatalf("head %d after the fill, want 6", r2.Head())
	}
	for n := uint32(1); n <= 6; n++ {
		got, err := r2.Block(n)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(blocks[n-1]) {
			t.Fatalf("position %d does not hold block %d", n, n)
		}
	}
}
