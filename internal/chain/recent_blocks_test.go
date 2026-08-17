package chain

import (
	"bytes"
	"testing"

	blockzstd "gopherd/internal/blocklog/zstd"
	"gopherd/internal/protocol/block"
)

func makeID(num uint32) block.ID {
	var id block.ID
	id[0] = byte(num >> 24)
	id[1] = byte(num >> 16)
	id[2] = byte(num >> 8)
	id[3] = byte(num)
	id[4] = 0xab
	return id
}

func TestRecentBlocksPutAndGet(t *testing.T) {
	r := NewRecentBlocks(4)
	id1 := makeID(1)
	r.Put(1, id1, []byte("first"))
	if got := r.ByNum(1); string(got) != "first" {
		t.Errorf("ByNum: got %q, want %q", got, "first")
	}
	if got := r.ByID(id1); string(got) != "first" {
		t.Errorf("ByID: got %q, want %q", got, "first")
	}
}

func TestRecentBlocksPutCompressedLazyDecode(t *testing.T) {
	dictNum, ok := blockzstd.LastAvailableDictNumber()
	if !ok {
		t.Skip("no dictionaries embedded")
	}
	original := bytes.Repeat([]byte("hello hive "), 64) // ~700 bytes; benefits from compression
	enc := blockzstd.NewCompressor()
	payload, err := enc.Compress(original, dictNum)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}

	r := NewRecentBlocks(4)
	id := makeID(42)
	r.PutCompressed(42, id, payload, dictNum, false)

	// Pre-read: no work done yet → 0 hits, 0 misses.
	hits, misses := r.DecompressionCacheStats()
	if hits != 0 || misses != 0 {
		t.Errorf("pre-read stats: hits=%d misses=%d (want 0,0)", hits, misses)
	}

	// First read materializes — count goes to misses=1.
	got := r.ByNum(42)
	if !bytes.Equal(got, original) {
		t.Fatalf("first ByNum: %d bytes, want %d (mismatch)", len(got), len(original))
	}
	hits, misses = r.DecompressionCacheStats()
	if misses != 1 || hits != 0 {
		t.Errorf("post-first-read stats: hits=%d misses=%d (want 0,1)", hits, misses)
	}

	// Second read hits the cached materialized form — hits=1, misses unchanged.
	got2 := r.ByID(id)
	if !bytes.Equal(got2, original) {
		t.Fatalf("second ByID: mismatch")
	}
	hits, misses = r.DecompressionCacheStats()
	if misses != 1 || hits != 1 {
		t.Errorf("post-second-read stats: hits=%d misses=%d (want 1,1)", hits, misses)
	}
}

func TestRecentBlocksEvictionPreservesByID(t *testing.T) {
	r := NewRecentBlocks(2)
	r.Put(1, makeID(1), []byte("a"))
	r.Put(2, makeID(2), []byte("b"))
	r.Put(3, makeID(3), []byte("c")) // evicts 1

	if got := r.ByNum(1); got != nil {
		t.Errorf("expected eviction of block 1, got %q", got)
	}
	if got := r.ByID(makeID(1)); got != nil {
		t.Errorf("expected ByID(1) evicted, got %q", got)
	}
	if got := r.ByNum(2); string(got) != "b" {
		t.Errorf("ByNum(2): got %q want b", got)
	}
	if got := r.ByNum(3); string(got) != "c" {
		t.Errorf("ByNum(3): got %q want c", got)
	}
}

func TestRecentBlocksReplaceSameNum(t *testing.T) {
	r := NewRecentBlocks(2)
	id1 := makeID(7)
	r.Put(7, id1, []byte("v1"))
	id2 := makeID(7)
	id2[5] = 0xff // distinct id under same num (forks scenario)
	r.Put(7, id2, []byte("v2"))

	if got := r.ByID(id1); got != nil {
		t.Errorf("old id should be unreachable after replace, got %q", got)
	}
	if got := r.ByID(id2); string(got) != "v2" {
		t.Errorf("new id: got %q want v2", got)
	}
	if got := r.ByNum(7); string(got) != "v2" {
		t.Errorf("ByNum(7): got %q want v2", got)
	}
}
