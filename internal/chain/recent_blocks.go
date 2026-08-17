package chain

import (
	"sync"
	"sync/atomic"

	blockzstd "gopherd/internal/blocklog/zstd"
	"gopherd/internal/protocol/block"
)

// RecentBlockCacheSize bounds how many of the most-recently-applied blocks
// we keep in memory for serving p2p sync requests. The historical chain
// is on disk in block_log; this cache covers the "last N produced" window
// where peers without on-disk blocklog (fresh restarts in tests) need to
// re-serve to a late-joiner.
//
// 200 blocks ~= 10 minutes of head — well past hived's
// `MAX_BLOCKS_PER_PEER_DURING_SYNCING` batch size of 200.
const RecentBlockCacheSize = 200

// recentEntry is one cache slot. exactly one of `raw` or `compressed` is
// always populated. When `compressed != nil`, `raw` is filled in lazily
// on first read and retained for subsequent hits (eager-decompress cache).
type recentEntry struct {
	id         block.ID
	raw        []byte // decompressed wire bytes (lazy when compressed)
	compressed []byte // magicless zstd payload (nil when source was uncompressed)
	dictNum    uint8  // dictionary number for the compressed form (ignored when noDict)
	noDict     bool   // compressed without a trained dictionary
	mu         sync.Mutex
}

// RecentBlocks is an in-memory ring of the last N applied blocks indexed
// by both block_num and block_id. ApplyBlock writes to it; the p2p sync
// layer reads from it.
//
// Entries can be stored in two forms: raw uncompressed (cheapest reads,
// largest footprint) or zstd-compressed-magicless (used when the source
// block arrived compressed, saving decompression on the receive path
// only to re-decompress on each subsequent read). The compressed form is
// transparently materialized on first read and cached in-place; second +
// reads of the same block_num/id pay nothing extra.
//
// Thread-safe: Apply writes under a goroutine separate from the readers.
type RecentBlocks struct {
	mu       sync.RWMutex
	cap      int
	entries  map[uint32]*recentEntry
	byID     map[block.ID]*recentEntry
	idForNum map[uint32]block.ID
	numOrder []uint32 // FIFO of block_nums for eviction

	// decompressor is lazily allocated on the first compressed Put. nil
	// means no compressed entry has ever been stored (so Get paths can
	// skip the decompressor entirely).
	decompressor atomic.Pointer[blockzstd.Decompressor]

	// hit-rate counters for the lazy decompression cache. Cumulative
	// across the lifetime of this RecentBlocks.
	decompressHits   atomic.Uint64
	decompressMisses atomic.Uint64
}

// NewRecentBlocks builds an empty cache with the given capacity. capacity<=0
// disables caching (Get returns nil; Put is a no-op).
func NewRecentBlocks(capacity int) *RecentBlocks {
	if capacity <= 0 {
		return &RecentBlocks{cap: 0}
	}
	return &RecentBlocks{
		cap:      capacity,
		entries:  make(map[uint32]*recentEntry, capacity),
		byID:     make(map[block.ID]*recentEntry, capacity),
		idForNum: make(map[uint32]block.ID, capacity),
		numOrder: make([]uint32, 0, capacity),
	}
}

// Put records the raw block bytes for blockNum (with derived block id id).
// Evicts the oldest entry if at capacity. Caller must NOT mutate raw after.
func (r *RecentBlocks) Put(blockNum uint32, id block.ID, raw []byte) {
	if r == nil || r.cap == 0 {
		return
	}
	r.putEntry(blockNum, id, &recentEntry{id: id, raw: raw})
}

// PutCompressed records a magicless zstd-compressed block under blockNum
// (id is the canonical 20-byte block_id). The decompressed bytes are NOT
// computed at insert time; they are produced on the first ByNum/ByID hit
// using the dictionary identified by dictNum (or no dictionary when
// noDict is true) and cached in-place. Subsequent reads return the cached
// uncompressed slice with no decompression work.
//
// This is the storage form for blocks that arrive over the wire as
// compressed_block_message — letting the recent-block ring keep more
// blocks in the same memory budget when the workload is dominated by
// compressed inbound traffic.
func (r *RecentBlocks) PutCompressed(blockNum uint32, id block.ID, compressed []byte, dictNum uint8, noDict bool) {
	if r == nil || r.cap == 0 {
		return
	}
	if r.decompressor.Load() == nil {
		// First compressed insert — allocate the shared decompressor.
		// Race-tolerant: losers' allocations get dropped by CAS.
		r.decompressor.CompareAndSwap(nil, blockzstd.NewDecompressor())
	}
	r.putEntry(blockNum, id, &recentEntry{
		id:         id,
		compressed: compressed,
		dictNum:    dictNum,
		noDict:     noDict,
	})
}

// putEntry is the common map-update path shared by Put + PutCompressed.
func (r *RecentBlocks) putEntry(blockNum uint32, id block.ID, e *recentEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[blockNum]; ok {
		// Replace under the same num — drop the prior id->entry mapping.
		if oldID, ok := r.idForNum[blockNum]; ok && oldID != id {
			delete(r.byID, oldID)
		}
		r.entries[blockNum] = e
		r.byID[id] = e
		r.idForNum[blockNum] = id
		return
	}
	if len(r.numOrder) >= r.cap {
		// Evict the oldest.
		evict := r.numOrder[0]
		r.numOrder = r.numOrder[1:]
		if oldID, ok := r.idForNum[evict]; ok {
			delete(r.byID, oldID)
		}
		delete(r.entries, evict)
		delete(r.idForNum, evict)
	}
	r.entries[blockNum] = e
	r.byID[id] = e
	r.idForNum[blockNum] = id
	r.numOrder = append(r.numOrder, blockNum)
}

// materialize returns the entry's decompressed bytes, decompressing +
// caching them on first call. Returns nil on decode error (the cache slot
// is left intact so a future caller can re-attempt or callers can fall
// back to fetching from disk).
func (r *RecentBlocks) materialize(e *recentEntry) []byte {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.raw != nil {
		r.decompressHits.Add(1)
		return e.raw
	}
	if e.compressed == nil {
		return nil
	}
	dec := r.decompressor.Load()
	if dec == nil {
		// Defensive: PutCompressed always allocates this. If we got
		// here without one, we can't safely decode.
		return nil
	}
	var (
		out []byte
		err error
	)
	if e.noDict {
		out, err = dec.DecompressNoDict(e.compressed, nil)
	} else {
		out, err = dec.Decompress(e.compressed, e.dictNum, nil)
	}
	if err != nil {
		return nil
	}
	e.raw = out
	r.decompressMisses.Add(1)
	return e.raw
}

// ByNum returns the cached raw block bytes for blockNum, or nil if absent.
func (r *RecentBlocks) ByNum(blockNum uint32) []byte {
	if r == nil || r.cap == 0 {
		return nil
	}
	r.mu.RLock()
	e := r.entries[blockNum]
	r.mu.RUnlock()
	return r.materialize(e)
}

// ByID returns the cached raw block bytes for the given block id, or nil
// if absent.
func (r *RecentBlocks) ByID(id block.ID) []byte {
	if r == nil || r.cap == 0 {
		return nil
	}
	r.mu.RLock()
	e := r.byID[id]
	r.mu.RUnlock()
	return r.materialize(e)
}

// DecompressionCacheStats returns (hits, misses) on the lazy
// decompression cache. hits is incremented on every read of an already-
// materialized compressed entry (or any uncompressed entry); misses is
// incremented exactly once per compressed entry on its first read (the
// decompression-actually-happened path). Useful for diagnostics +
// rate-limiting decisions.
func (r *RecentBlocks) DecompressionCacheStats() (hits, misses uint64) {
	if r == nil {
		return 0, 0
	}
	return r.decompressHits.Load(), r.decompressMisses.Load()
}

// IDForNum returns the cached block ID corresponding to blockNum, with
// ok=true when the mapping is present.
func (r *RecentBlocks) IDForNum(blockNum uint32) (block.ID, bool) {
	if r == nil || r.cap == 0 {
		return block.ID{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.idForNum[blockNum]
	return id, ok
}

// RecentNums returns the block_nums currently held, in insertion order
// (oldest first). Returned slice is a copy safe for caller modification.
func (r *RecentBlocks) RecentNums() []uint32 {
	if r == nil || r.cap == 0 {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]uint32, len(r.numOrder))
	copy(out, r.numOrder)
	return out
}

// Len returns how many blocks are currently cached.
func (r *RecentBlocks) Len() int {
	if r == nil || r.cap == 0 {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.numOrder)
}
