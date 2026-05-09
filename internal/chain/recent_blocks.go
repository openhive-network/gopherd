package chain

import (
	"sync"

	"hived-go/internal/protocol/block"
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

// RecentBlocks is an in-memory ring of the last N applied blocks indexed
// by both block_num and block_id. ApplyBlock writes to it; the p2p sync
// layer reads from it.
//
// Thread-safe: Apply writes under a goroutine separate from the readers.
type RecentBlocks struct {
	mu        sync.RWMutex
	cap       int
	byNum     map[uint32][]byte
	byID      map[block.ID][]byte
	idForNum  map[uint32]block.ID
	numOrder  []uint32 // FIFO of block_nums for eviction
}

// NewRecentBlocks builds an empty cache with the given capacity. capacity<=0
// disables caching (Get returns nil; Put is a no-op).
func NewRecentBlocks(capacity int) *RecentBlocks {
	if capacity <= 0 {
		return &RecentBlocks{cap: 0}
	}
	return &RecentBlocks{
		cap:      capacity,
		byNum:    make(map[uint32][]byte, capacity),
		byID:     make(map[block.ID][]byte, capacity),
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byNum[blockNum]; ok {
		// Replace under the same num — drop the prior id->bytes mapping.
		if oldID, ok := r.idForNum[blockNum]; ok && oldID != id {
			delete(r.byID, oldID)
		}
		// Reuse buffer; no need to re-append numOrder.
		r.byNum[blockNum] = raw
		r.byID[id] = raw
		r.idForNum[blockNum] = id
		_ = existing
		return
	}
	if len(r.numOrder) >= r.cap {
		// Evict the oldest.
		evict := r.numOrder[0]
		r.numOrder = r.numOrder[1:]
		if oldID, ok := r.idForNum[evict]; ok {
			delete(r.byID, oldID)
		}
		delete(r.byNum, evict)
		delete(r.idForNum, evict)
	}
	r.byNum[blockNum] = raw
	r.byID[id] = raw
	r.idForNum[blockNum] = id
	r.numOrder = append(r.numOrder, blockNum)
}

// ByNum returns the cached raw block bytes for blockNum, or nil if absent.
func (r *RecentBlocks) ByNum(blockNum uint32) []byte {
	if r == nil || r.cap == 0 {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byNum[blockNum]
}

// ByID returns the cached raw block bytes for the given block id, or nil
// if absent.
func (r *RecentBlocks) ByID(id block.ID) []byte {
	if r == nil || r.cap == 0 {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byID[id]
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
