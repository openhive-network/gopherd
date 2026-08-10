package sync

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	blockzstd "hived-go/internal/blocklog/zstd"
	"hived-go/internal/chain"
	"hived-go/internal/crypto"
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
	// is 10000; we used to cap at 100 to keep individual responses bounded
	// (hived sends one block_message per item asynchronously, so a 10K
	// fetch generates 10K replies, not one big one). 500 keeps the per-RTT
	// bookkeeping cheap while letting catch-up pipeline more deeply per
	// peer — the per-peer outstanding cap (MaxOutstandingPerPeer = 512)
	// is the real ceiling.
	DefaultFetchBatch = 500

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
	// 16384-deep queue caps worst-case in-flight memory at ~1 GiB even
	// during a multi-peer fan-out storm. Combined with the high-water
	// back-pressure below it should never get close to that on the happy
	// path; it's the upper guardrail. Bumped from 4096 to 16384 to keep
	// the queue out of the way during a multi-peer catch-up where 8
	// peers each push thousands of blocks in flight.
	DefaultApplyQueueSize = 16384

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
	//
	// 1024 turned out too small for sustained historical catch-up: peers
	// deliver compressed blocks faster than the single-writer applyLoop
	// chews them, the buffer fills, and every further block is evicted
	// then RE-FETCHED — burning bandwidth and capping throughput at
	// roughly the apply-rate × (1 - re-fetch overhead). Bumped to 16384
	// (=MaxOutstandingTotal) so the buffer can absorb a full
	// in-flight wave without dropping.
	DefaultMaxPendingReorder = 16384

	// DefaultMaxOutstandingTotal caps the total number of blocks we have
	// in flight (fetched but not yet applied) across all peers. Together
	// with ApplyQueueSize this is the upper bound on memory consumed by
	// inbound block traffic.
	DefaultMaxOutstandingTotal = 8192

	// DefaultMaxOutstandingPerPeer caps how many blocks any single peer
	// can have in flight to us. Keeps a single misbehaving (or simply
	// faster) peer from monopolising the budget.
	DefaultMaxOutstandingPerPeer = 2048

	// DefaultCatchupKickInterval is the cadence at which the catchup-
	// kicker loop pokes every connected peer with a fresh
	// fetch_blockchain_item_ids while isFarBehind() is true. The
	// passive walk-forward only fires after a peer answers the prior
	// request — slow peers stall their own pipeline. The kicker fills
	// the stall by re-requesting independently of peer responses,
	// keeping multiple chunks in flight per peer.
	DefaultCatchupKickInterval = 1 * time.Second
)

// advertEntry is one block queued for advertisement: its id plus the block
// timestamp we saw at apply time. Carrying the timestamp means the advert path
// never has to go back to the chain to judge freshness.
type advertEntry struct {
	id  block.ID
	num uint32
	ts  uint32 // block timestamp (unix seconds)
}

// Chain is the slice of *chain.Database the sync layer needs. Allows
// tests to plug in a stub.
type Chain interface {
	HeadBlock() (uint32, error)
	HeadBlockID() (block.ID, error)
	ApplyBlock(rawBlock []byte, blockNum uint32, opts chain.ApplyOptions, stats *chain.Stats) (interface{}, error)
	PushLiveBlock(rawBlock []byte, blockNum uint32, opts chain.ApplyOptions) (chain.PushOutcome, error)
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
func (c *chainAdapter) PushLiveBlock(raw []byte, n uint32, opts chain.ApplyOptions) (chain.PushOutcome, error) {
	return c.db.PushLiveBlock(raw, n, opts)
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
	// CatchupKickInterval is the cadence at which the catch-up kicker
	// re-issues fetch_blockchain_item_ids to every peer while we're
	// far behind. 0 → DefaultCatchupKickInterval.
	CatchupKickInterval time.Duration
	// Logger receives one-line status messages. nil → os.Stderr.
	Logger io.Writer

	// Mempool, when non-nil, receives inbound transaction bodies. Supplying it
	// also switches ON transaction fetching: with no mempool we record tx
	// inventory as known and never request the bodies, since there would be
	// nowhere to put them.
	//
	// *witness.Plugin satisfies this. Without it a producing node builds empty
	// blocks — which is exactly what gopherd did until 2026-08-10: it took its
	// slot and contributed zero transactions, dumping the backlog on the next
	// witness.
	Mempool Mempool

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

	// LogEveryBlock, when true, emits one "p2p/sync: applied block N"
	// line per applied block. Off by default — the chain layer's
	// per-second progress line is sufficient for normal catch-up
	// operation and the per-block writes were measurably slowing
	// down a fast multi-thousand-bps sync (stderr formatting + write
	// syscall overhead dominated). Tests that grep the per-block line
	// can opt back in.
	LogEveryBlock bool

	// AdaptiveFetchBatch toggles per-peer FetchBatch sizing in
	// handleBlockchainItemIDsInventory. With it ON (default true via
	// Config.defaults), each fetch chunk is sized to the peer's recent
	// blocks/second so fast peers get big batches and slow peers stay
	// small. With it OFF, every peer uses cfg.FetchBatch — useful for
	// regression bisects.
	AdaptiveFetchBatch bool
	// adaptiveFetchBatchSet tracks whether the caller explicitly set
	// AdaptiveFetchBatch. Without this, defaults() can't tell "false
	// because zero-value" apart from "false because caller wants it
	// off"; we pin the default to ON unless the caller said otherwise.
	adaptiveFetchBatchSet bool

	// LogAdaptiveBatch, when true, emits one trace line per per-peer
	// FetchBatch decision ("peer X batch=N rate=Y bps"). Useful for
	// tuning + as the visible signal in catch-up logs that adaptive
	// sizing is engaged. Off by default — high-rate runs would spam
	// hundreds of lines per second.
	LogAdaptiveBatch bool

	// Serve controls whether this node ADVERTISES its inventory and SERVES
	// blocks/ids to peers. DEFAULT false (zero value) = a purely passive,
	// fetch-only leecher: it dials peers and downloads blocks but NEVER runs
	// broadcastInv and NEVER answers a peer's fetch_items / fetch_ids request.
	//
	// This is a safety default: an unfinished/underperforming consensus node
	// that advertises an item and then can't serve it promptly (e.g. while
	// GC-stalled or wedged at the tip) gets dropped by peers as
	// out-of-contact/malicious, and repeated across the peer set that churn
	// can disrupt the wider network (see the 2026-07-09 mainnet incident:
	// "advertising an item and then not providing it when asked"). A passive
	// node cannot cause that failure mode. Set true only for a node that is
	// known-correct, keeps up in real time, and is meant to relay.
	Serve bool
	// LogWorkTiming, when true, emits a periodic per-block work-timing line
	// ("apply avg=Xms p50=Y p99=Z over N blocks; serve avg=Wms/req") so
	// slowness is measurable and comparable to hived's block-processing timing.
	LogWorkTiming bool

	// ForkAware routes inbound blocks through chain.PushLiveBlock (the
	// fork-choice engine: extend / buffer / depth-1 tip reorg) instead of the
	// strictly-linear head+1 ApplyBlock. DEFAULT false — the linear path is
	// unchanged for replay/catch-up. Enable for a tip-follower that must ride
	// the live head through micro-forks. The node must also wire
	// SetOnBlockFinalized so block_log-append + eviction fire only on commit.
	ForkAware bool
}

// EnableAdaptiveFetchBatch is a setter for AdaptiveFetchBatch that
// also flips adaptiveFetchBatchSet so defaults() doesn't override the
// caller's choice. Use this when you want to explicitly DISABLE
// adaptive sizing (the default is ON).
func (c *Config) EnableAdaptiveFetchBatch(on bool) {
	c.AdaptiveFetchBatch = on
	c.adaptiveFetchBatchSet = true
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
	if c.CatchupKickInterval == 0 {
		c.CatchupKickInterval = DefaultCatchupKickInterval
	}
	if c.Logger == nil {
		c.Logger = os.Stderr
	}
	// Default adaptive FetchBatch ON unless the caller explicitly
	// disabled it via EnableAdaptiveFetchBatch(false).
	if !c.adaptiveFetchBatchSet {
		c.AdaptiveFetchBatch = true
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

	// txAccepted / txRejected count mempool admissions. Both matter: all-zero
	// accepted with non-zero rejected means we are fetching transactions but
	// validating them wrongly, which looks identical to a quiet network if only
	// one is tracked.
	txAccepted atomic.Uint64
	txRejected atomic.Uint64
	// txInvOffered / txFetched bracket the step BEFORE admission. Without them,
	// "accepted=0, rejected=0" is ambiguous between three different faults:
	// peers never advertise transactions to us, we never ask for the bodies, or
	// the bodies never arrive. Each needs a different fix, and they look
	// identical from outside.
	txInvOffered atomic.Uint64
	txFetched    atomic.Uint64
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
	// invIgnoredCatchup counts item_ids_inventory_message adverts dropped by
	// handleInv's catch-up gate (the hived-1.28.7 storm fix). It climbs while
	// we're behind and flatlines the instant we reach the live tip — a direct,
	// observable signal that the gate is doing its job and self-clearing.
	invIgnoredCatchup atomic.Uint64

	// compressed-block accounting. compressedReceived counts every
	// compressed_block_message that decoded cleanly (regardless of whether
	// it was a duplicate of an already-applied block). compressedDecompressed
	// counts how many of those required actual zstd decompression (cache
	// miss), and compressedCacheHits counts the pre-apply duplicate path
	// where the same blockID arrived again before applyLoop caught up and
	// we were able to skip the zstd work entirely.
	compressedReceived      atomic.Uint64
	compressedDecompressed  atomic.Uint64
	compressedCacheHits     atomic.Uint64
	compressedDecodeErrors  atomic.Uint64

	// decompressor + recentCompressed cache for the inbound
	// compressed_block_message path. The decompressor is shared across all
	// peer-receive goroutines (it is internally synchronised). The cache
	// is a tiny FIFO of the most recent compressed-block IDs we've seen,
	// so a duplicate compressed delivery (peer A and peer B both pushing
	// the same compressed_block to us during catch-up) doesn't redo the
	// zstd work — we drop the duplicate by ID and skip everything past
	// the 2-byte parse + cache hit accounting.
	decompressor   *blockzstd.Decompressor
	recentCompMu   sync.Mutex
	recentCompSeen map[block.ID]struct{}
	recentCompFIFO []block.ID

	// outstandingTotal is the cumulative in-flight fetch count across
	// all peers. Incremented when we add a hash to st.outstanding,
	// decremented when we receive the matching block, drop it via
	// item_not_available, or evict it on queue-full.
	outstandingTotal atomic.Int32

	// inflight is the GLOBAL set of block IDs currently requested from
	// some peer, keyed by block ID. Without it, the catch-up kicker
	// (which re-asks every peer "what's after my head?" once a second)
	// makes us request the same next ~2000 blocks from every peer in
	// parallel — measured ~4x redundant downloads, all of which must be
	// decompressed and then discarded. The global set lets us fetch each
	// block from ONE peer; a stale entry (holder slow for > reRequestStale)
	// is reassigned so a single slow/silent peer can't head-of-line block
	// the in-order applyLoop. Guarded by inflightMu, which is always
	// acquired LAST (after d.mu / st.mu) to keep a strict lock order.
	inflightMu sync.Mutex
	inflight   map[block.ID]inflightEntry

	// tipStallKicks counts how many times the kicker re-seeded because we were
	// stalled AT THE TIP (head not advancing, nothing in flight) rather than
	// because isCatchingUp() was true. Surfaced in Stats so a live node shows
	// whether it is silently sawtoothing behind the head. Steady 0 at a healthy
	// tip; climbing = blocks are being dropped at head+1.
	tipStallKicks atomic.Int64

	// invReceived counts inbound item_ids_inventory messages (tip INV pushes
	// from peers), incremented on ENTRY to handleInv before any gate. It exists
	// to answer one question that nothing else could: at the tip, do peers
	// advertise new blocks to us at all? A serving node observed 2026-08-09
	// advanced ONLY via the catchup kicker's poll, and neither invIgnored (the
	// catch-up gate) nor the "TRACE inv-offer" line could distinguish "no
	// adverts arrive" from "adverts arrive and are discarded" — that trace is
	// in handleBlockchainItemIDsInventory (the kicker path), despite its name.
	invReceived atomic.Int64

	// tipSyncHandshakes counts fetch_blockchain_item_ids rounds issued purely
	// to clear the peers' peer_needs_sync_items_from_us flag while we sit level
	// at the tip. Paired with invReceived: handshakes climbing while invRx
	// stays 0 means the handshake is not convincing peers we are in sync.
	tipSyncHandshakes atomic.Int64

	// pendingAdvertMu guards pendingAdvert.
	//
	// pendingAdvert is the direct analogue of hived's _new_block_inventory
	// (node.cpp): blocks we have APPLIED since the last advert round, pushed in
	// at apply time and drained wholesale each round. Being event-driven is the
	// point — the set can only ever contain blocks we just accepted, so
	// staleness is structural rather than something we filter for afterwards.
	// The previous design derived the advert set by scanning the recent-block
	// ring, which is what let ~10 minutes of history reach new peers.
	pendingAdvertMu sync.Mutex
	pendingAdvert   []advertEntry

	// lastAdvertisedNum is the highest block number we have offered via
	// item_ids_inventory. broadcastInv only advertises blocks ABOVE it, so a
	// freshly-connected peer receives the same small new-block stream as
	// everyone else instead of a backlog of the recent ring. See the incident
	// note in broadcastInv.
	lastAdvertisedNum atomic.Uint32

	// traceLo/traceHi define an optional block-number window for verbose
	// per-block ingestion tracing (GHIVE_TRACE_LO/HI). Zero = disabled.
	traceLo uint32
	traceHi uint32

	// signerCache dedupes RecoverBlockSigner across peer-receive
	// goroutines. During catch-up the same block frequently arrives
	// from multiple peers; without the cache, every duplicate triggers
	// a fresh secp256k1 recovery (the dominant CPU cost on the hot path).
	signerCache *signerCache
	// txSigCache is the per-tx analogue of signerCache: it memoizes the
	// per-block per-tx signer-pubkey set produced by the look-ahead
	// recovery pool, so duplicate block deliveries don't re-pay the
	// (multi-millisecond on a 100-tx block) parallel recovery cost.
	txSigCache *txSigCache

	// headBlockHint is an atomic mirror of d.chain.HeadBlock(), updated
	// after each successful apply. handleBlock reads it lock-free to
	// fast-skip duplicate deliveries; calling d.chain.HeadBlock() on
	// every inbound block was costing ~2ms per call (acquires applyMu
	// + materialises a state.Session) and serialising the per-peer
	// receive goroutines. Lagging by one block is harmless on this path.
	headBlockHint atomic.Uint32

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

	// blocksReceived counts blocks delivered by THIS peer (regardless
	// of whether the block ultimately applied or was a duplicate).
	// Atomic so handleBlock can bump it without taking st.mu — avoids
	// contending with broadcastInv / handleInv on the hot path.
	blocksReceived atomic.Uint64
	// rateWindowStart is the wall-clock nanos of the last rate sample.
	// adaptiveBatchSize() reads it and rolls a new window every
	// rateWindow ns; the resulting blocks/second feeds the per-peer
	// FetchBatch sizing in handleBlockchainItemIDsInventory.
	rateWindowStart   atomic.Int64
	rateWindowBlocks  atomic.Uint64
	// lastRate is the most-recent blocks/second sample (rounded down to
	// uint32). Recomputed once per rateWindow; FetchBatch sizing reads
	// it lock-free. Initialized to 0; the adaptive sizer treats 0 as
	// "no sample yet, use baseline batch".
	lastRate atomic.Uint32
}

func newPeerState() *peerState {
	st := &peerState{
		known:       make(map[block.ID]struct{}, 256),
		outstanding: make(map[block.ID]struct{}, 64),
	}
	st.rateWindowStart.Store(time.Now().UnixNano())
	return st
}

// rateWindow is how often we snapshot per-peer block-receive rate. Short
// enough that a slow peer's bad streak shows up in its next batch sizing
// (~30s mirrors the alpha2 spec hint). Long enough that a single
// unlucky-RTT round doesn't flap the size.
const rateWindow = 30 * time.Second

// minAdaptiveBatch / maxAdaptiveBatch bracket per-peer FetchBatch sizing.
// We keep maxAdaptive below MaxOutstandingPerPeer so a single round
// never blows past the per-peer outstanding cap.
const (
	minAdaptiveBatch = 50
	maxAdaptiveBatch = 5000
)

// recordBlock bumps both lifetime and current-window counters. Cheap:
// two atomic adds on the hot per-peer receive path. Caller is
// handleBlock; called once per received block_message.
func (st *peerState) recordBlock() {
	st.blocksReceived.Add(1)
	st.rateWindowBlocks.Add(1)
}

// adaptiveBatchSize returns a per-peer FetchBatch size scaled to the
// peer's recent blocks/second. A peer that's been delivering 1500 bps
// gets a 5000-batch chunk so we keep its pipeline full; a peer dribbling
// 50 bps is capped at the floor so we don't waste 5000 slots on it.
//
// Sampling is pull-based: the function rolls the rate window only when
// it's older than rateWindow. This means we only do the wall-clock read
// + division once per ~30s per peer per call site, rather than every
// time recordBlock fires. The trade is a slightly stale rate reading on
// the next FetchBatch decision (max one window stale) — fine, since
// FetchBatch decisions only happen on inventory-reply boundaries.
//
// baseline is the global cfg.FetchBatch — used as the floor when we
// don't yet have any per-peer rate sample.
func (st *peerState) adaptiveBatchSize(baseline int) int {
	now := time.Now().UnixNano()
	start := st.rateWindowStart.Load()
	elapsed := now - start
	if elapsed >= int64(rateWindow) {
		// Roll the window: snapshot the count, recompute lastRate, reset.
		blocks := st.rateWindowBlocks.Swap(0)
		st.rateWindowStart.Store(now)
		secs := float64(elapsed) / float64(time.Second)
		var bps uint32
		if secs > 0 {
			bps = uint32(float64(blocks) / secs)
		}
		st.lastRate.Store(bps)
	}
	rate := st.lastRate.Load()
	if rate == 0 {
		// First window or peer has gone silent — use the baseline batch
		// (cfg.FetchBatch) so the very first round still ramps quickly.
		if baseline < minAdaptiveBatch {
			return minAdaptiveBatch
		}
		if baseline > maxAdaptiveBatch {
			return maxAdaptiveBatch
		}
		return baseline
	}
	// Scale: aim for ~3 seconds of work per batch. A peer at 1000 bps
	// gets a 3000-block batch; at 200 bps → 600; at 2000+ bps → capped
	// at maxAdaptiveBatch so a single round never overruns the per-peer
	// outstanding budget.
	size := int(rate) * 3
	if size < minAdaptiveBatch {
		size = minAdaptiveBatch
	}
	if size > maxAdaptiveBatch {
		size = maxAdaptiveBatch
	}
	return size
}

// LastRate returns the most-recent blocks/second sample for this peer.
// Exported for observability + tests.
func (st *peerState) LastRate() uint32 { return st.lastRate.Load() }

// reRequestStale is how long a block may sit requested-but-undelivered
// from one peer before another peer is allowed to fetch it too. Short
// enough that a slow/silent peer can't stall the in-order applyLoop for
// long (the next block must arrive before head can advance); long enough
// that the common sub-second delivery never triggers a redundant re-ask.
const reRequestStale = 2 * time.Second

// inflightEntry records which peer we're currently fetching a block from
// and when the request was (re)issued, for the staleness check.
type inflightEntry struct {
	peer    *node.Peer
	sinceNs int64
}

// tryReserveInflight attempts to claim bid for peer p. Returns true if p
// should fetch it — either bid was unclaimed, or the current holder has
// been slow for longer than reRequestStale (so we let a second peer race
// it). Returns false when another peer holds a fresh reservation, so the
// caller skips the redundant fetch. nowNs is passed in so a single round
// shares one clock read. Acquired under no other lock that would violate
// the d.mu > st.mu > inflightMu order.
func (d *Delegate) tryReserveInflight(bid block.ID, p *node.Peer, nowNs int64) bool {
	d.inflightMu.Lock()
	defer d.inflightMu.Unlock()
	e, ok := d.inflight[bid]
	if !ok {
		d.inflight[bid] = inflightEntry{peer: p, sinceNs: nowNs}
		return true
	}
	if e.peer != p && nowNs-e.sinceNs >= int64(reRequestStale) {
		d.inflight[bid] = inflightEntry{peer: p, sinceNs: nowNs}
		return true
	}
	return false
}

// removeInflight clears bid's global reservation. Idempotent. Called when
// the block arrives (from anyone), is reported unavailable, is dropped on
// queue-full, or the fetch is abandoned under back-pressure.
func (d *Delegate) removeInflight(bid block.ID) {
	d.inflightMu.Lock()
	delete(d.inflight, bid)
	d.inflightMu.Unlock()
}

// inTrace reports whether n is inside the configured trace window.
func (d *Delegate) inTrace(n uint32) bool {
	return d.traceHi != 0 && n >= d.traceLo && n <= d.traceHi
}

// diagnoseAwaited scans every tracking structure for block numbers in
// [n, n+3] and reports where (if anywhere) the awaited block is held.
// Stall diagnostics only — O(peers*outstanding + inflight + compRing).
func (d *Delegate) diagnoseAwaited(n uint32) string {
	hi := n + 3
	// Global inflight.
	d.inflightMu.Lock()
	var infl []string
	nowNs := time.Now().UnixNano()
	for bid, e := range d.inflight {
		if bn := bid.Num(); bn >= n && bn <= hi {
			infl = append(infl, fmt.Sprintf("%d(age=%dms)", bn, (nowNs-e.sinceNs)/1e6))
		}
	}
	d.inflightMu.Unlock()
	// Compressed dedup ring.
	d.recentCompMu.Lock()
	var comp []uint32
	for bid := range d.recentCompSeen {
		if bn := bid.Num(); bn >= n && bn <= hi {
			comp = append(comp, bn)
		}
	}
	d.recentCompMu.Unlock()
	// Per-peer outstanding.
	d.mu.Lock()
	out := 0
	for _, st := range d.peers {
		st.mu.Lock()
		for bid := range st.outstanding {
			if bn := bid.Num(); bn >= n && bn <= hi {
				out++
			}
		}
		st.mu.Unlock()
	}
	d.mu.Unlock()
	return fmt.Sprintf("inflight=%v compRing=%v outstandingPeers=%d", infl, comp, out)
}

// removeInflightPeer drops every reservation held by p (peer disconnected),
// so the blocks it owed become immediately re-requestable from others.
func (d *Delegate) removeInflightPeer(p *node.Peer) {
	d.inflightMu.Lock()
	for bid, e := range d.inflight {
		if e.peer == p {
			delete(d.inflight, bid)
		}
	}
	d.inflightMu.Unlock()
}

// applyJob is one inbound block waiting for the applier goroutine.
// Mempool is the transaction sink. Implementations validate and store; a
// returned error means "not usable", which is ordinary traffic rather than a
// fault.
type Mempool interface {
	AddPendingRaw(body []byte) error
}

type applyJob struct {
	blockNum uint32
	rawBlock []byte
	id       block.ID
	srcPeer  *node.Peer
	// preVerifiedSigner, when non-zero, is the witness signing key
	// recovered from the block's WitnessSignature on the per-peer
	// receive goroutine (which is parallel across peers, unlike the
	// single applyLoop). chain.ApplyBlock skips its own RecoverPublicKey
	// when this is set, eliminating the dominant CPU cost from the
	// hot apply path.
	preVerifiedSigner crypto.PublicKey
	// preRecoveredTxKeys, when non-nil, is the per-tx signer-pubkey set
	// recovered ahead of the apply on the look-ahead worker pool. Slot
	// i corresponds to tx i in the block; nil slots fall through to
	// chain.ApplyBlock's serial recovery (preserving canonical error
	// messages for non-canonical / mis-signed txs). Combined with the
	// per-block fan-out inside RecoverBlockTxSignerKeys, this lifts the
	// dominant secp256k1 CPU cost out of the single-writer applyLoop.
	preRecoveredTxKeys [][]crypto.PublicKey
	// local marks a block WE produced (via PublishBlock). We signed it
	// microseconds ago, so re-recovering the witness signature is pure waste —
	// and it lands in the worst possible place: inside the 3-second slot budget,
	// between building the block and advertising it. Only the BLOCK signature is
	// skipped; transaction signatures are still verified, since txs come from
	// the mempool rather than from us.
	local bool
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
		signerCache:     newSignerCache(),
		decompressor:    blockzstd.NewDecompressor(),
		recentCompSeen:  make(map[block.ID]struct{}, recentCompressedCap),
		recentCompFIFO:  make([]block.ID, 0, recentCompressedCap),
		txSigCache:      newTxSigCache(),
		inflight:        make(map[block.ID]inflightEntry, 8192),
	}
	if lo := os.Getenv("GHIVE_TRACE_LO"); lo != "" {
		if v, err := strconv.ParseUint(lo, 10, 32); err == nil {
			d.traceLo = uint32(v)
		}
	}
	if hi := os.Getenv("GHIVE_TRACE_HI"); hi != "" {
		if v, err := strconv.ParseUint(hi, 10, 32); err == nil {
			d.traceHi = uint32(v)
		}
	}
	return d
}

// recentCompressedCap bounds the in-flight dedup ring for
// compressed_block_message arrivals. Sized well above
// MaxOutstandingPerPeer (default 2048) × the typical 8 peers so a
// peer fan-out of the same compressed block during catch-up can't
// thrash the ring. 256 was too small — under sustained catch-up with
// multiple peers each holding ~2k blocks in flight, the ring evicted
// before all peers' duplicates could be filtered, defeating the
// dedup and re-paying decompression for the same block.
const recentCompressedCap = 8192

// Start launches the background goroutines: the INV broadcaster, the
// single-consumer block applier, and the catch-up kicker.
// Idempotent.
func (d *Delegate) Start() {
	d.wg.Add(4)
	go d.invLoop()
	go d.applyLoop()
	go d.catchupKickerLoop()
	go d.pruneLoop()
}

// Stop signals the background goroutines and waits for them to exit.
func (d *Delegate) Stop() {
	close(d.stopCh)
	d.wg.Wait()
}

// NotifyHeadAdvanced should be called by the witness/gossip layer
// whenever a new local block is applied. Wakes the INV loop if it is
// in idle-sleep mode.
// NotifyHeadAdvanced wakes the loops. It deliberately does NOT queue anything
// for advertisement: blocks enter the advert set only from the apply path via
// queueAdvert, so the set stays a record of what we actually accepted. Adding a
// chain read here would also make this a lock-ordering hazard for any future
// caller that holds a chain lock.
func (d *Delegate) NotifyHeadAdvanced() {
	d.poke()
}

// PublishBlock injects a block WE just produced into the SAME apply queue that
// inbound blocks use, so the single applyLoop goroutine applies it via
// chain.PushLiveBlock and then advertises it via queueAdvert. The witness
// plugin calls this INSTEAD of applying and broadcasting the block itself.
//
// Both halves of that matter, and both were mainnet bugs on 2026-08-10 (block
// 108887429, howo's slot):
//
//  1. PUBLICATION. The witness used to hand the raw block to
//     gossip.BroadcastBlock, which pushed an unsolicited block_message body to
//     every peer. hived only accepts a block_message it explicitly requested —
//     libraries/net/node.cpp: "received a block I didn't ask for from peer,
//     disconnecting from peer". All 8 peers dropped us in the same instant, the
//     block never propagated, and howo missed the slot. hived's own
//     node_impl::broadcast(full_block) never pushes a body: it caches the block
//     for serving, inserts it into _new_block_inventory and triggers the
//     advertise loop, so peers learn the block ID and fetch the body. Routing
//     through the apply path puts us on exactly that mechanism, since
//     queueAdvert is our advertise-by-ID equivalent and the applied block is
//     then serve-able from the chain's recent-block cache.
//
//  2. REVERSIBILITY. The witness used to call chain.ApplyBlock directly, with
//     no Reversible flag, so a block we produced became the COMMITTED head the
//     instant we made it. Nothing can reorg away a committed block: when the
//     network's competing block for the same height arrived, PushLiveBlock saw
//     num <= committedNum() and filed it PushStale, and its child buffered
//     forever awaiting a parent we would never apply. The node wedged at its own
//     orphan for as long as it was left running, with --fork-switching ON and
//     powerless. Going through PushLiveBlock keeps our own tip REVERSIBLE, which
//     is the whole precondition for fork choice to be able to undo it.
//
// Returns an error rather than blocking if the queue is full — the caller is on
// a 3-second slot budget and a produced block that cannot be enqueued promptly
// is better reported than silently delayed.
func (d *Delegate) PublishBlock(rawBlock []byte, blockNum uint32) error {
	_, hdrLen, err := block.UnmarshalSignedHeader(rawBlock)
	if err != nil {
		return fmt.Errorf("publish block %d: header: %w", blockNum, err)
	}
	bid := block.ConstructID(rawBlock[:hdrLen], blockNum)
	// srcPeer nil marks this as locally produced: the applyLoop then has no
	// peer to exclude from the advertisement, which is correct — we want every
	// peer to learn our block ID.
	job := applyJob{
		blockNum: blockNum,
		rawBlock: rawBlock,
		id:       bid,
		srcPeer:  nil,
		local:    true,
	}
	select {
	case d.applyQ <- job:
		d.onEnqueue()
		d.poke()
		return nil
	default:
		return fmt.Errorf("publish block %d: apply queue full (%d/%d)",
			blockNum, d.applyQLen.Load(), cap(d.applyQ))
	}
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
	// CompressedReceived is every compressed_block_message we decoded
	// (including duplicates we then short-circuited via the per-ID cache).
	CompressedReceived uint64
	// CompressedDecompressed is the subset that actually went through zstd
	// decompression. Difference from Received is the cache-hit count.
	CompressedDecompressed uint64
	// CompressedCacheHits counts how many times a duplicate compressed
	// arrival was short-circuited by recent-block / dedup cache.
	CompressedCacheHits uint64
	// CompressedDecodeErrors counts decode failures (bad flags, dict not
	// embedded, zstd error, header parse). Useful as a sanity gauge.
	CompressedDecodeErrors uint64
	// PeerCount is the number of connected peers with per-peer state.
	PeerCount int
	// DistinctOutstanding is the number of UNIQUE block IDs currently in
	// flight (a block requested from N peers via the walk-forward fan-out
	// counts N times in OutstandingTotal but once here). The ratio
	// OutstandingTotal/DistinctOutstanding reveals fetch redundancy.
	DistinctOutstanding int
	// GlobalInflight is the size of the global in-flight reservation set.
	// With dedup working it tracks DistinctOutstanding closely.
	GlobalInflight int
	// TxAccepted / TxRejected count mempool admissions, and MempoolDepth is
	// the current queue. All three exist because a producing node's mempool is
	// otherwise INVISIBLE: gopherd produced empty blocks for its whole life
	// without a single metric saying so. Read them together — TxAccepted=0 with
	// TxRejected>0 means transactions arrive and validation rejects them all,
	// which looks exactly like a quiet network if you only track one.
	TxAccepted   uint64
	TxRejected   uint64
	MempoolDepth int
	// TxInvOffered / TxFetched bracket the step before admission — see the
	// delegate fields for why all four are needed to localise a fault.
	TxInvOffered uint64
	TxFetched    uint64
	// InvIgnoredCatchup is the cumulative count of tip-inventory adverts
	// dropped by handleInv's catch-up gate (hived-1.28.7 storm fix). Nonzero
	// and climbing == we're behind and correctly ignoring tip INV; stops
	// climbing once we reach the live tip.
	InvIgnoredCatchup uint64

	// TipStallKicks is the cumulative count of tip-stall re-seeds (head stuck
	// with nothing in flight while at the tip). 0 on a healthy live node;
	// climbing means blocks are being dropped at head+1 and we are sawtoothing.
	TipStallKicks int64

	// InvReceived is the cumulative count of inbound tip-inventory adverts.
	// At the live tip this should climb steadily (peers announcing new blocks).
	// Flat at 0 while the head advances only via tipStallKicks means we are
	// polling for blocks, not being pushed them.
	InvReceived int64

	// TipSyncHandshakes is the cumulative count of level-at-tip
	// fetch_blockchain_item_ids rounds sent to clear peers' out-of-sync flag.
	TipSyncHandshakes int64
}

func (d *Delegate) distinctOutstanding() (distinct, peers int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	seen := make(map[block.ID]struct{}, 1024)
	for _, st := range d.peers {
		st.mu.Lock()
		for bid := range st.outstanding {
			seen[bid] = struct{}{}
		}
		st.mu.Unlock()
	}
	return len(seen), len(d.peers)
}

func (d *Delegate) Stats() Stats {
	distinct, peers := d.distinctOutstanding()
	d.inflightMu.Lock()
	inflight := len(d.inflight)
	d.inflightMu.Unlock()
	mempoolDepth := 0
	if mp, ok := d.cfg.Mempool.(interface{ PendingCount() int }); ok && mp != nil {
		mempoolDepth = mp.PendingCount()
	}
	return Stats{
		QueueDepth:             d.QueueDepth(),
		QueueCapacity:          d.QueueCapacity(),
		FetchInhibited:         d.FetchInhibited(),
		OutstandingTotal:       int(d.outstandingTotal.Load()),
		DroppedQueueFull:       d.droppedQueueFull.Load(),
		DroppedReorder:         d.droppedReorder.Load(),
		InhibitedFetches:       d.inhibitedFetches.Load(),
		OutstandingThrottled:   d.outstandingThrottled.Load(),
		CompressedReceived:     d.compressedReceived.Load(),
		CompressedDecompressed: d.compressedDecompressed.Load(),
		CompressedCacheHits:    d.compressedCacheHits.Load(),
		CompressedDecodeErrors: d.compressedDecodeErrors.Load(),
		PeerCount:              peers,
		DistinctOutstanding:    distinct,
		GlobalInflight:         inflight,
		InvIgnoredCatchup:      d.invIgnoredCatchup.Load(),
		TipStallKicks:          d.tipStallKicks.Load(),
		InvReceived:            d.invReceived.Load(),
		TipSyncHandshakes:      d.tipSyncHandshakes.Load(),
		TxAccepted:             d.txAccepted.Load(),
		TxRejected:             d.txRejected.Load(),
		TxInvOffered:           d.txInvOffered.Load(),
		TxFetched:              d.txFetched.Load(),
		MempoolDepth:           mempoolDepth,
	}
}
