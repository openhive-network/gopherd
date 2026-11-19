package zstd

import (
	"runtime"
	"testing"
	"time"
)

// TestPooledDCtxFreedWhenDropped pins that a libzstd DCtx is freed when the
// sync.Pool holding its decoder drops it at a GC. The C context is invisible to
// the Go collector, so before the decoder carried a cleanup every GC leaked the
// pooled DCtxs: a from-genesis replay held 3.2 GB of C heap by block 22M and
// was OOM-killed under a 20 GB cap near 58M.
func TestPooledDCtxFreedWhenDropped(t *testing.T) {
	d := NewDecompressor()
	defer d.Close()
	num, ok := LastAvailableDictNumber()
	if !ok {
		t.Skip("no embedded dictionary")
	}
	orig := make([]byte, 8192)
	for i := range orig {
		orig[i] = byte(i * 31)
	}
	comp, err := NewCompressor().Compress(orig, num)
	if err != nil {
		t.Skipf("compressor unavailable: %v", err)
	}

	base := liveDCtxs.Load()
	const rounds = 500
	for i := 0; i < rounds; i++ {
		out, err := d.Decompress(comp, num, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != len(orig) {
			t.Fatalf("length: got %d want %d", len(out), len(orig))
		}
		// Two collections empty both the pool and its victim cache, so the
		// next Decompress builds a fresh DCtx and the old one is dropped.
		runtime.GC()
		runtime.GC()
	}
	// Cleanups run on their own goroutine after the collection that finds
	// the wrapper unreachable.
	deadline := time.Now().Add(10 * time.Second)
	for liveDCtxs.Load()-base > 8 && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if n := liveDCtxs.Load() - base; n > 8 {
		t.Fatalf("%d DCtxs still live after %d pool drops; dropped decoders are not freed", n, rounds)
	}

	// Close frees the pooled DCtxs at once; a decoder the pool dropped
	// before Close may still be waiting for its cleanup.
	d.Close()
	deadline = time.Now().Add(10 * time.Second)
	for liveDCtxs.Load() != base && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if n := liveDCtxs.Load() - base; n != 0 {
		t.Fatalf("%d DCtxs live after Close", n)
	}
}
