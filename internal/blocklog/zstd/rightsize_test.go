package zstd

import "testing"

// TestDecompressReturnsRightSizedSlice pins that a decompressed block does not
// pin a backing array far larger than itself.
//
// decompress allocates at least 64 KiB (doubling to 16 MiB) but a mainnet block
// is typically 5-15 KiB. Returning a slice INTO that buffer means anything
// retaining the block — the recent-block ring, the fork db, an in-flight apply
// job — retains the whole buffer. Measured at 2.52 GB of live heap (49.8% of
// the process) after 8h at the tip, ~62 KB retained per block against the
// 64 KiB floor.
func TestDecompressReturnsRightSizedSlice(t *testing.T) {
	d := NewDecompressor()
	defer d.Close()

	// A small payload, like a quiet mainnet block.
	orig := make([]byte, 4096)
	for i := range orig {
		orig[i] = byte(i * 31)
	}
	c := NewCompressor()
	comp, err := c.CompressNoDict(orig)
	if err != nil {
		t.Skipf("compressor unavailable: %v", err)
	}
	out, err := d.DecompressNoDict(comp, nil)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if len(out) != len(orig) {
		t.Fatalf("length: got %d want %d", len(out), len(orig))
	}
	for i := range orig {
		if out[i] != orig[i] {
			t.Fatalf("content differs at %d", i)
		}
	}
	// The point of the fix: capacity must be close to length, not the 64 KiB
	// allocation floor.
	if cap(out) > 2*len(out)+8192 {
		t.Fatalf("returned slice pins %d bytes for a %d byte block — the "+
			"backing buffer is being retained", cap(out), len(out))
	}
}
