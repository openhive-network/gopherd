package sync

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"hived-go/internal/chain"
	"hived-go/internal/p2p/node"
	"hived-go/internal/protocol/block"
)

// Defaults — tunable via Config.
const (
	// DefaultINVInterval is how often each node broadcasts its
	// item_ids_inventory_message advertising recently-produced blocks
	// and recently-received txs. Mirrors hived's advertise_inventory_loop
	// cadence (~5 seconds).
	DefaultINVInterval = 5 * time.Second

	// DefaultFetchBatch is the maximum number of item IDs we put on a
	// single fetch_items_message. Hived's GRAPHENE_NET_MAX_ITEMS_PER_BATCH
	// is 10000; we cap lower to keep individual responses bounded.
	DefaultFetchBatch = 100

	// DefaultMaxBlocksPerInv bounds how many block IDs we list in one
	// item_ids_inventory_message. Hived advertises blocks one-by-one
	// as they arrive; we batch the last N for efficiency on a slow
	// catch-up where the late-joiner missed many.
	DefaultMaxBlocksPerInv = 200

	// DefaultMaxTxPerInv bounds the tx half of an item_ids_inventory.
	DefaultMaxTxPerInv = 100

	// DefaultIdleINVRounds — after this many consecutive INV rounds where
	// the node has nothing new to advertise AND no peer responded with
	// anything new, we stop broadcasting until the next time something
	// changes (a new block applies or a peer reports a new head).
	// Avoids steady-state INV spam on a quiet chain.
	DefaultIdleINVRounds = 6
)

// Chain is the slice of *chain.Database the sync layer needs. Allows
// tests to plug in a stub.
type Chain interface {
	HeadBlock() (uint32, error)
	HeadBlockID() (block.ID, error)
	ApplyBlock(rawBlock []byte, blockNum uint32, opts chain.ApplyOptions, stats *chain.Stats) (interface{}, error)
	RecentBlockBytes(blockNum uint32) []byte
	RecentBlockBytesByID(id block.ID) []byte
	RecentBlockNums() []uint32
	RecentBlockIDForNum(blockNum uint32) (block.ID, bool)
}

// chainAdapter wraps a real *chain.Database in our narrow Chain
// interface (the ApplyBlock return type differs).
type chainAdapter struct {
	db *chain.Database
}

func (c *chainAdapter) HeadBlock() (uint32, error)                   { return c.db.HeadBlock() }
func (c *chainAdapter) HeadBlockID() (block.ID, error)               { return c.db.HeadBlockID() }
func (c *chainAdapter) RecentBlockBytes(n uint32) []byte             { return c.db.RecentBlockBytes(n) }
func (c *chainAdapter) RecentBlockBytesByID(id block.ID) []byte      { return c.db.RecentBlockBytesByID(id) }
func (c *chainAdapter) RecentBlockNums() []uint32                    { return c.db.RecentBlockNums() }
func (c *chainAdapter) RecentBlockIDForNum(n uint32) (block.ID, bool) {
	return c.db.RecentBlockIDForNum(n)
}
func (c *chainAdapter) ApplyBlock(raw []byte, n uint32, opts chain.ApplyOptions, stats *chain.Stats) (interface{}, error) {
	return c.db.ApplyBlock(raw, n, opts, stats)
}

// Config tunes the sync delegate.
type Config struct {
	// INVInterval is the cadence for the periodic item_ids_inventory broadcast.
	INVInterval time.Duration
	// FetchBatch caps the number of items we put on one fetch_items_message.
	FetchBatch int
	// MaxBlocksPerInv caps the block half of an item_ids_inventory.
	MaxBlocksPerInv int
	// MaxTxPerInv caps the tx half of an item_ids_inventory.
	MaxTxPerInv int
	// IdleINVRounds caps consecutive rounds of "nothing to advertise" before
	// we sleep until something changes. 0 = never sleep.
	IdleINVRounds int
	// Logger receives one-line status messages. nil → os.Stderr.
	Logger io.Writer
}

func (c *Config) defaults() {
	if c.INVInterval == 0 {
		c.INVInterval = DefaultINVInterval
	}
	if c.FetchBatch == 0 {
		c.FetchBatch = DefaultFetchBatch
	}
	if c.MaxBlocksPerInv == 0 {
		c.MaxBlocksPerInv = DefaultMaxBlocksPerInv
	}
	if c.MaxTxPerInv == 0 {
		c.MaxTxPerInv = DefaultMaxTxPerInv
	}
	if c.IdleINVRounds == 0 {
		c.IdleINVRounds = DefaultIdleINVRounds
	}
	if c.Logger == nil {
		c.Logger = os.Stderr
	}
}

// Delegate implements node.Delegate for catch-up sync. Cheap to
// construct; only one per *node.Node.
type Delegate struct {
	cfg   Config
	chain Chain

	// peers tracks the per-peer state map. Lifecycle: insert on
	// OnPeerConnected, drop on OnPeerDisconnected.
	mu    sync.Mutex
	peers map[*node.Peer]*peerState

	// applyQ is a single-consumer queue of inbound blocks waiting to
	// be applied via chain.ApplyBlock. The applier goroutine is the
	// only writer to chain state from the sync layer.
	applyQ    chan applyJob
	applyDone chan struct{}

	// idleRounds counts consecutive INV broadcasts that produced no
	// outgoing items and got no fresh items from peers. Resets on any
	// activity. Once it crosses cfg.IdleINVRounds we stop the periodic
	// broadcast until activityCh fires.
	idleMu       sync.Mutex
	idleRounds   int
	activityCh   chan struct{}

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// peerState tracks the per-peer dedup + outstanding-fetch sets.
type peerState struct {
	mu sync.Mutex
	// known is the set of item hashes we've either advertised TO this
	// peer or received FROM this peer. Used to dedup outbound INV.
	known map[block.ID]struct{}
	// outstanding tracks block IDs we've requested from this peer but
	// not yet received. Lets the catch-up controller avoid asking the
	// same peer twice.
	outstanding map[block.ID]struct{}
	// peerHeadNum is the last head_block_num we observed from this
	// peer (via blockchain_item_ids_inventory or block arrivals).
	peerHeadNum uint32
}

func newPeerState() *peerState {
	return &peerState{
		known:       make(map[block.ID]struct{}, 256),
		outstanding: make(map[block.ID]struct{}, 64),
	}
}

// applyJob is one inbound block waiting for the applier goroutine.
type applyJob struct {
	blockNum uint32
	rawBlock []byte
	id       block.ID
	srcPeer  *node.Peer
}

// New builds a Delegate against a real *chain.Database.
func New(cfg Config, db *chain.Database) *Delegate {
	cfg.defaults()
	return newWithChain(cfg, &chainAdapter{db: db})
}

// newWithChain is the test-friendly constructor.
func newWithChain(cfg Config, c Chain) *Delegate {
	d := &Delegate{
		cfg:        cfg,
		chain:      c,
		peers:      make(map[*node.Peer]*peerState, 16),
		applyQ:     make(chan applyJob, 256),
		applyDone:  make(chan struct{}),
		activityCh: make(chan struct{}, 1),
		stopCh:     make(chan struct{}),
	}
	return d
}

// Start launches the background goroutines: the INV broadcaster and
// the single-consumer block applier. Idempotent.
func (d *Delegate) Start() {
	d.wg.Add(2)
	go d.invLoop()
	go d.applyLoop()
}

// Stop signals the background goroutines and waits for them to exit.
func (d *Delegate) Stop() {
	close(d.stopCh)
	d.wg.Wait()
}

// NotifyHeadAdvanced should be called by the witness/gossip layer
// whenever a new local block is applied. Wakes the INV loop if it is
// in idle-sleep mode.
func (d *Delegate) NotifyHeadAdvanced() {
	d.poke()
}

func (d *Delegate) poke() {
	d.idleMu.Lock()
	d.idleRounds = 0
	d.idleMu.Unlock()
	select {
	case d.activityCh <- struct{}{}:
	default:
	}
}

func (d *Delegate) logf(format string, args ...interface{}) {
	if d.cfg.Logger == nil {
		return
	}
	fmt.Fprintf(d.cfg.Logger, "p2p/sync: "+format+"\n", args...)
}
