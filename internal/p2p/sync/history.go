package sync

import (
	"fmt"
	"sync"
	"sync/atomic"

	"gopherd/internal/blocklog"
	"gopherd/internal/protocol/block"
)

// BlockHistory serves sync requests for blocks older than the recent-block
// ring. hived answers them from its block log (fork_db_block_reader::
// get_block_ids reads the ids below the fork database out of the block log,
// and block_log_wrapper::fetch_block_by_id reads the block at the id's number
// and returns it only if its id matches). With a BlockHistory configured the
// delegate does the same; without one it serves the ring alone.
type BlockHistory interface {
	// BlockIDForNum returns the id of the block this node holds at n, or
	// ok=false when n is outside the range it serves.
	BlockIDForNum(n uint32) (block.ID, bool)
	// BlockBytesByID returns the raw signed block whose id is exactly id, or
	// nil. Never a different block at the same height.
	BlockBytesByID(id block.ID) []byte
}

// HistoryMaxBlocksPerInv caps the ids in one blockchain_item_ids_inventory
// reply when a BlockHistory backs it. hived lists up to 10000
// (DEFAULT_MAX_BLOCK_IDS_TO_FETCH, net/node.hpp), reading them from the
// block_log artifacts file, which stores every block id. Our block_log has no
// id column, so each id costs a block read, decompress and hash: measured on
// a mainnet block_log from a cold page cache, ~7 ms per 1000 ids at 1M and
// ~150 ms per 1000 in the dense HF17+ era. 2000 keeps a reply near 0.3 s,
// well inside hived's 6 s active_ignored_request_timeout; the requester asks
// again for the remainder.
const HistoryMaxBlocksPerInv = 2000

// blocksPerLogPart is hived's split block_log part size
// (BLOCKS_IN_SPLIT_BLOCK_LOG_FILE, 1M blocks per block_log_part.NNNN).
const blocksPerLogPart = 1_000_000

// BlockLogHistory is a BlockHistory over the node's own block_log, which a
// Writer in the same process keeps appending to.
//
// keep mirrors hived's block-log-split option (chain_plugin.cpp): -1 serves
// the whole log; N > 0 serves the part holding the newest block plus the N
// full million-block parts before it, which is what a hived node configured
// with block-log-split=N still has on disk.
type BlockLogHistory struct {
	mu   sync.Mutex
	r    *blocklog.Reader
	keep int

	idsServed    atomic.Uint64
	blocksServed atomic.Uint64
	readErrors   atomic.Uint64
}

// NewBlockLogHistory wraps r. keep must be -1 or > 0.
func NewBlockLogHistory(r *blocklog.Reader, keep int) (*BlockLogHistory, error) {
	if r == nil {
		return nil, fmt.Errorf("block history: nil block_log reader")
	}
	if keep == 0 || keep < -1 {
		return nil, fmt.Errorf("block history: keep %d: want -1 (whole block_log) or N > 0 (latest N million-block parts)", keep)
	}
	return &BlockLogHistory{r: r, keep: keep}, nil
}

// lowestServed returns the first block number served when the log ends at
// head (the newest block, whose part is always kept).
func (h *BlockLogHistory) lowestServed(head uint32) uint32 {
	if h.keep < 0 || head == 0 {
		return 1
	}
	headPart := (head-1)/blocksPerLogPart + 1
	if uint32(h.keep) >= headPart {
		return 1
	}
	return (headPart-uint32(h.keep)-1)*blocksPerLogPart + 1
}

// readLocked returns the raw block at n and its id. Only blocks strictly below
// the reader's Head are read: the newest indexed block's end is not settled
// until the block after it is indexed (see blocklog.Reader.Refresh). Those are
// in the recent ring anyway.
func (h *BlockLogHistory) readLocked(n uint32) ([]byte, block.ID, bool) {
	if n == 0 {
		return nil, block.ID{}, false
	}
	if n >= h.r.Head() {
		if err := h.r.Refresh(); err != nil {
			h.readErrors.Add(1)
			return nil, block.ID{}, false
		}
		if n >= h.r.Head() {
			return nil, block.ID{}, false
		}
	}
	if n < h.lowestServed(h.r.Head()) {
		return nil, block.ID{}, false
	}
	raw, err := h.r.Block(n)
	if err != nil {
		h.readErrors.Add(1)
		return nil, block.ID{}, false
	}
	id, ok := blockIDFromRaw(raw)
	if !ok || id.Num() != n {
		h.readErrors.Add(1)
		return nil, block.ID{}, false
	}
	return raw, id, true
}

// BlockIDForNum implements BlockHistory.
func (h *BlockLogHistory) BlockIDForNum(n uint32) (block.ID, bool) {
	h.mu.Lock()
	_, id, ok := h.readLocked(n)
	h.mu.Unlock()
	if ok {
		h.idsServed.Add(1)
	}
	return id, ok
}

// BlockBytesByID implements BlockHistory.
func (h *BlockLogHistory) BlockBytesByID(id block.ID) []byte {
	h.mu.Lock()
	raw, got, ok := h.readLocked(id.Num())
	h.mu.Unlock()
	if !ok || got != id {
		return nil
	}
	h.blocksServed.Add(1)
	return raw
}

// HistoryStats is a snapshot of BlockLogHistory's counters.
type HistoryStats struct {
	IDsRead      uint64 // block ids read from the block_log (sync-id replies, ancestor checks)
	BlocksServed uint64 // blocks served to peers from the block_log
	ReadErrors   uint64 // reads that failed or did not decode
}

// Stats returns the counters. Safe on a nil receiver.
func (h *BlockLogHistory) Stats() HistoryStats {
	if h == nil {
		return HistoryStats{}
	}
	return HistoryStats{
		IDsRead:      h.idsServed.Load(),
		BlocksServed: h.blocksServed.Load(),
		ReadErrors:   h.readErrors.Load(),
	}
}

// holdsBlockID reports whether id is a block this node can serve: in the
// recent ring, or (with a History) in the block_log at its number with exactly
// that id.
func (d *Delegate) holdsBlockID(id block.ID) bool {
	if d.chain.RecentBlockBytesByID(id) != nil {
		return true
	}
	if d.cfg.History == nil {
		return false
	}
	got, ok := d.cfg.History.BlockIDForNum(id.Num())
	return ok && got == id
}

// servableBlockIDForNum returns the id of the main-chain block at n: from the
// ring, else from the History.
func (d *Delegate) servableBlockIDForNum(n uint32) (block.ID, bool) {
	if id, ok := d.chain.RecentBlockIDForNum(n); ok {
		return id, true
	}
	if d.cfg.History == nil {
		return block.ID{}, false
	}
	return d.cfg.History.BlockIDForNum(n)
}

// servableBlockBytes returns the raw block with exactly this id: from the
// ring, else from the History.
func (d *Delegate) servableBlockBytes(id block.ID) []byte {
	if raw := d.chain.RecentBlockBytesByID(id); raw != nil {
		return raw
	}
	if d.cfg.History == nil {
		return nil
	}
	return d.cfg.History.BlockBytesByID(id)
}
