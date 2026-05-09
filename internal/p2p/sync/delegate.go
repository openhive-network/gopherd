package sync

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
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

	// DefaultApplyQueueSize bounds the inbound block channel feeding the
	// single applyLoop. Mainnet blocks are ≤ ~64 KiB on the wire so a
	// 4096-deep queue caps worst-case in-flight memory at ~256 MiB even
	// during a multi-peer fan-out storm. Combined with the high-water
	// back-pressure below it should never get close to that on the happy
	// path; it's the upper guardrail.
	DefaultApplyQueueSize = 4096

	// DefaultApplyQueueHighWaterPct — when the apply queue passes this
	// fraction of capacity we tell every peer to STOP sending us blocks
	// (we stop issuing fetch_items). Mirrors hived's
	// _inhibit_fetching_sync_blocks_until per-peer hysteresis.
	DefaultApplyQueueHighWaterPct = 75

	// DefaultApplyQueueLowWaterPct — once the queue drains below this
	// fraction we resume issuing fetch_items. The gap between high and
	// low forms a hysteresis band so we don't oscillate every block.
	DefaultApplyQueueLowWaterPct = 25

	// DefaultMaxPendingReorder caps the in-applyLoop reorder buffer (the
	// `pending` map keyed by block_num). If we get more than this many
	// out-of-order future blocks before the missing predecessor arrives
	// we drop the highest-num entries — those will be re-fetched via INV
	// once the gap closes. Without this guardrail an adversarial peer
	// streaming far-future block IDs could OOM us.
	DefaultMaxPendingReorder = 1024

	// DefaultMaxOutstandingTotal caps the total number of blocks we have
	// in flight (fetched but not yet applied) across all peers. Together
	// with ApplyQueueSize this is the upper bound on memory consumed by
	// inbound block traffic. Set to about half the apply queue so a peer
	// stream burst can't fully saturate the queue alone.
	DefaultMaxOutstandingTotal = 2048

	// DefaultMaxOutstandingPerPeer caps how many blocks any single peer
	// can have in flight to us. Keeps a single misbehaving (or simply
	// faster) peer from monopolising the budget.
	DefaultMaxOutstandingPerPeer = 512
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
	// ApplyQueueSize is the buffer depth of the inbound applyQ channel.
	// 0 → DefaultApplyQueueSize.
	ApplyQueueSize int
	// ApplyQueueHighWaterPct triggers per-peer back-pressure (we stop
	// issuing fetch_items) when the queue passes this fraction full.
	// 0 → DefaultApplyQueueHighWaterPct.
	ApplyQueueHighWaterPct int
	// ApplyQueueLowWaterPct lifts back-pressure once the queue drains
	// back under this fraction. Must be < high-water for hysteresis.
	// 0 → DefaultApplyQueueLowWaterPct.
	ApplyQueueLowWaterPct int
	// MaxPendingReorder caps the in-applyLoop reorder buffer.
	// 0 → DefaultMaxPendingReorder.
	MaxPendingReorder int
	// MaxOutstandingTotal caps total in-flight fetches across all peers.
	// 0 → DefaultMaxOutstandingTotal.
	MaxOutstandingTotal int
	// MaxOutstandingPerPeer caps in-flight fetches to a single peer.
	// 0 → DefaultMaxOutstandingPerPeer.
	MaxOutstandingPerPeer int
	// Logger receives one-line status messages. nil → os.Stderr.
	Logger io.Writer

	// OnBlockApplied, when non-nil, is invoked from the applyLoop AFTER
	// each successful chain.ApplyBlock. The gossip layer wires its
	// BroadcastAppliedBlock here so that sync remains the single owner
	// of ApplyBlock yet the same goroutine that committed the block
	// also fans it out to peers.
	//
	// rawBlock is the FC-binary signed_block bytes (no trailing
	// block_id). source is the peer the block arrived from, or nil for
	// blocks we applied ourselves (currently unused by sync — gossip's
	// outbound BroadcastBlock from witness still goes through the
	// gossip layer directly, NOT through this hook).
	OnBlockApplied func(rawBlock []byte, blockNum uint32, source *node.Peer)
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
	if c.ApplyQueueSize == 0 {
		c.ApplyQueueSize = DefaultApplyQueueSize
	}
	if c.ApplyQueueHighWaterPct == 0 {
		c.ApplyQueueHighWaterPct = DefaultApplyQueueHighWaterPct
	}
	if c.ApplyQueueLowWaterPct == 0 {
		c.ApplyQueueLowWaterPct = DefaultApplyQueueLowWaterPct
	}
	if c.ApplyQueueLowWaterPct >= c.ApplyQueueHighWaterPct {
		// Defensive: callers might set high without low. Fall back to a
		// 50% gap so we still have hysteresis.
		c.ApplyQueueLowWaterPct = c.ApplyQueueHighWaterPct / 2
		if c.ApplyQueueLowWaterPct < 1 {
			c.ApplyQueueLowWaterPct = 1
		}
	}
	if c.MaxPendingReorder == 0 {
		c.MaxPendingReorder = DefaultMaxPendingReorder
	}
	if c.MaxOutstandingTotal == 0 {
		c.MaxOutstandingTotal = DefaultMaxOutstandingTotal
	}
	if c.MaxOutstandingPerPeer == 0 {
		c.MaxOutstandingPerPeer = DefaultMaxOutstandingPerPeer
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

	// applyQLen is an atomic mirror of len(applyQ) maintained by the
	// enqueue/dequeue paths. Letting handler goroutines poll a uint32
	// is much cheaper than racing on the channel; it's used by the
	// back-pressure check in sendFetchItems / handleInv.
	applyQLen atomic.Int32
	// inhibitFetching is set when applyQLen >= high-water and cleared
	// when it drops below low-water. While set, sendFetchItems is a
	// no-op. Toggle is performed by the applyLoop on each dequeue and
	// by handleBlock on each enqueue.
	inhibitFetching atomic.Bool
	// applyQHighWater / applyQLowWater are the absolute slot counts
	// derived from cfg percentages, computed once at construction.
	applyQHighWater int32
	applyQLowWater  int32

	// idleRounds counts consecutive INV broadcasts that produced no
	// outgoing items and got no fresh items from peers. Resets on any
	// activity. Once it crosses cfg.IdleINVRounds we stop the periodic
	// broadcast until activityCh fires.
	idleMu       sync.Mutex
	idleRounds   int
	activityCh   chan struct{}

	// dropCounters track soft load-shedding events; exposed for
	// observability + tests.
	droppedQueueFull   atomic.Uint64
	droppedReorder     atomic.Uint64
	inhibitedFetches   atomic.Uint64
	outstandingThrottled atomic.Uint64

	// outstandingTotal is the cumulative in-flight fetch count across
	// all peers. Incremented when we add a hash to st.outstanding,
	// decremented when we receive the matching block, drop it via
	// item_not_available, or evict it on queue-full.
	outstandingTotal atomic.Int32

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
	// Tests construct Config literals directly without going through New;
	// fill in defaults here so MaxOutstanding* etc. are non-zero.
	cfg.defaults()
	cap := cfg.ApplyQueueSize
	if cap <= 0 {
		cap = DefaultApplyQueueSize
	}
	hi := int32(cap) * int32(cfg.ApplyQueueHighWaterPct) / 100
	lo := int32(cap) * int32(cfg.ApplyQueueLowWaterPct) / 100
	if hi <= 0 {
		hi = int32(cap)
	}
	if lo < 0 {
		lo = 0
	}
	d := &Delegate{
		cfg:             cfg,
		chain:           c,
		peers:           make(map[*node.Peer]*peerState, 16),
		applyQ:          make(chan applyJob, cap),
		applyDone:       make(chan struct{}),
		activityCh:      make(chan struct{}, 1),
		stopCh:          make(chan struct{}),
		applyQHighWater: hi,
		applyQLowWater:  lo,
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

// onEnqueue updates the atomic queue length + arms the inhibit flag if
// we crossed the high-water mark. Called once per successful enqueue
// in handleBlock.
func (d *Delegate) onEnqueue() {
	n := d.applyQLen.Add(1)
	if n >= d.applyQHighWater && !d.inhibitFetching.Load() {
		if d.inhibitFetching.CompareAndSwap(false, true) {
			d.logf("apply queue at %d/%d (>= high-water %d) — inhibiting fetches",
				n, cap(d.applyQ), d.applyQHighWater)
		}
	}
}

// onDequeue mirrors onEnqueue from the applyLoop. Lifts the inhibit
// flag once we drain back below the low-water mark.
func (d *Delegate) onDequeue() {
	n := d.applyQLen.Add(-1)
	if n <= d.applyQLowWater && d.inhibitFetching.Load() {
		if d.inhibitFetching.CompareAndSwap(true, false) {
			d.logf("apply queue at %d/%d (<= low-water %d) — resuming fetches",
				n, cap(d.applyQ), d.applyQLowWater)
			// Wake the INV loop so we re-broadcast our (advanced) head
			// and resume the catch-up promptly.
			d.poke()
		}
	}
}

// FetchInhibited reports whether sendFetchItems is currently
// back-pressured. Exported for tests + observability.
func (d *Delegate) FetchInhibited() bool { return d.inhibitFetching.Load() }

// QueueDepth returns the current applyQ depth (atomic, lock-free).
func (d *Delegate) QueueDepth() int { return int(d.applyQLen.Load()) }

// QueueCapacity returns the configured applyQ slot count.
func (d *Delegate) QueueCapacity() int { return cap(d.applyQ) }

// Stats returns a snapshot of the cumulative load-shedding counters.
type Stats struct {
	QueueDepth           int
	QueueCapacity        int
	FetchInhibited       bool
	OutstandingTotal     int
	DroppedQueueFull     uint64
	DroppedReorder       uint64
	InhibitedFetches     uint64
	OutstandingThrottled uint64
}

func (d *Delegate) Stats() Stats {
	return Stats{
		QueueDepth:           d.QueueDepth(),
		QueueCapacity:        d.QueueCapacity(),
		FetchInhibited:       d.FetchInhibited(),
		OutstandingTotal:     int(d.outstandingTotal.Load()),
		DroppedQueueFull:     d.droppedQueueFull.Load(),
		DroppedReorder:       d.droppedReorder.Load(),
		InhibitedFetches:     d.inhibitedFetches.Load(),
		OutstandingThrottled: d.outstandingThrottled.Load(),
	}
}
