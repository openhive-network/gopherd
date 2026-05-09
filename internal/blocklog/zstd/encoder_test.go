package zstd

import (
	"bytes"
	"testing"
)

// TestRoundTrip verifies that what we compress survives a Decompressor round-trip.
func TestRoundTrip(t *testing.T) {
	c := NewCompressor()
	d := NewDecompressor()
	defer d.Close()

	for n := range rawDicts {
		input := []byte("the quick brown fox jumps over the lazy dog. " +
			"this is a small payload that we'd expect to round-trip cleanly. " +
			"hived's mainnet blocks are typically a few KB each — we're proxying " +
			"that with a similar-shape string here.")
		out, err := c.Compress(input, n)
		if err != nil {
			t.Fatalf("compress with dict %d: %v", n, err)
		}
		if len(out) == 0 {
			t.Fatalf("empty output for dict %d", n)
		}
		got, err := d.Decompress(out, n, nil)
		if err != nil {
			t.Fatalf("decompress with dict %d: %v", n, err)
		}
		if !bytes.Equal(got, input) {
			t.Fatalf("round-trip mismatch for dict %d:\n got %q\nwant %q", n, got, input)
		}
	}
}

func TestRoundTripNoDict(t *testing.T) {
	c := NewCompressor()
	d := NewDecompressor()
	defer d.Close()
	input := []byte("hello world; no-dict round trip")
	out, err := c.CompressNoDict(input)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.DecompressNoDict(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, input) {
		t.Fatalf("got %q want %q", got, input)
	}
}

func TestBestDictNumberForBlock(t *testing.T) {
	// Mainnet has 10 dicts (0..9). At 8.2M blocks, the cap is dict 8 because
	// block_num/1M = 8 there, and dict 9 is reached only at block 9M+.
	cases := []struct {
		blockNum uint32
		want     uint8
	}{
		{1, 0},
		{999_999, 0},
		{1_000_000, 1},
		{1_000_001, 1},
		{8_999_999, 8},
		{9_000_000, 9},
		{20_000_000, 9}, // capped at last available
	}
	for _, c := range cases {
		got, ok := BestDictNumberForBlock(c.blockNum)
		if !ok {
			t.Fatalf("block %d: no dict available", c.blockNum)
		}
		if got != c.want {
			t.Errorf("block %d: got dict %d, want %d", c.blockNum, got, c.want)
		}
	}
}
