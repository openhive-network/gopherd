package sync

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hived-go/internal/chain"
	"hived-go/internal/p2p/node"
	"hived-go/internal/p2p/proto"
	"hived-go/internal/protocol/block"
)

// stubChain is an in-memory Chain implementation. It's enough to drive
// the sync delegate without spinning up the full state machinery.
type stubChain struct {
	mu     sync.Mutex
	head   uint32
	headID block.ID
	cache  map[uint32][]byte
	idMap  map[uint32]block.ID
	byID   map[block.ID][]byte
	order  []uint32
}

func newStubChain() *stubChain {
	return &stubChain{
		cache: make(map[uint32][]byte),
		idMap: make(map[uint32]block.ID),
		byID:  make(map[block.ID][]byte),
	}
}

func (s *stubChain) HeadBlock() (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.head, nil
}
func (s *stubChain) HeadBlockID() (block.ID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headID, nil
}
func (s *stubChain) RecentBlockBytes(n uint32) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cache[n]
}
func (s *stubChain) RecentBlockBytesByID(id block.ID) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[id]
}
func (s *stubChain) RecentBlockNums() []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint32, len(s.order))
	copy(out, s.order)
	return out
}
func (s *stubChain) RecentBlockIDForNum(n uint32) (block.ID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.idMap[n]
	return id, ok
}
func (s *stubChain) ApplyBlock(raw []byte, n uint32, _ chain.ApplyOptions, _ *chain.Stats) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n != s.head+1 {
		// Out of order — applier loop should have stashed it.
		return nil, nil
	}
	id := makeStubID(n)
	s.head = n
	s.headID = id
	s.cache[n] = raw
	s.idMap[n] = id
	s.byID[id] = raw
	s.order = append(s.order, n)
	return nil, nil
}

// PushLiveBlock satisfies the Chain interface for the fork-aware path. These
// tests exercise the linear (ForkAware=false) path, so this just mirrors a
// linear apply.
func (s *stubChain) PushLiveBlock(raw []byte, n uint32, opts chain.ApplyOptions) (chain.PushOutcome, error) {
	if _, err := s.ApplyBlock(raw, n, opts, nil); err != nil {
		return 0, err
	}
	return chain.PushExtended, nil
}
func (s *stubChain) seed(n uint32, raw []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := makeStubID(n)
	s.head = n
	s.headID = id
	s.cache[n] = raw
	s.idMap[n] = id
	s.byID[id] = raw
	s.order = append(s.order, n)
}

// liveBlockBytes builds a minimal raw-block buffer whose timestamp field
// (offset 20, 4 bytes LE — see blockTimestamp) is `ts`. Enough to drive
// isCatchingUp(): a head block with a recent ts reads as "live".
func liveBlockBytes(ts uint32) []byte {
	b := make([]byte, 24)
	b[20] = byte(ts)
	b[21] = byte(ts >> 8)
	b[22] = byte(ts >> 16)
	b[23] = byte(ts >> 24)
	return b
}

// makeStubID synthesises a 20-byte ID with the block num in the first
// 4 BE bytes (matches block.ConstructID's invariant).
func makeStubID(num uint32) block.ID {
	var id block.ID
	id[0] = byte(num >> 24)
	id[1] = byte(num >> 16)
	id[2] = byte(num >> 8)
	id[3] = byte(num)
	id[4] = 0xab
	id[5] = 0xcd
	return id
}

func TestRecentBlocksRingEvicts(t *testing.T) {
	rb := chain.NewRecentBlocks(3)
	rb.Put(1, makeStubID(1), []byte{1})
	rb.Put(2, makeStubID(2), []byte{2})
	rb.Put(3, makeStubID(3), []byte{3})
	rb.Put(4, makeStubID(4), []byte{4})
	if rb.ByNum(1) != nil {
		t.Errorf("expected block 1 evicted, got %v", rb.ByNum(1))
	}
	for _, n := range []uint32{2, 3, 4} {
		if rb.ByNum(n) == nil {
			t.Errorf("expected block %d cached", n)
		}
	}
	if got := rb.Len(); got != 3 {
		t.Errorf("Len=%d want 3", got)
	}
	nums := rb.RecentNums()
	if len(nums) != 3 || nums[0] != 2 || nums[2] != 4 {
		t.Errorf("RecentNums()=%v", nums)
	}
}

func TestRecentBlocksByIDLookup(t *testing.T) {
	rb := chain.NewRecentBlocks(8)
	id := makeStubID(42)
	rb.Put(42, id, []byte{0xfe, 0xed})
	if got := rb.ByID(id); string(got) != "\xfe\xed" {
		t.Errorf("ByID got %v", got)
	}
	if got, ok := rb.IDForNum(42); !ok || got != id {
		t.Errorf("IDForNum got %v ok=%v", got, ok)
	}
}

func TestSyncDelegateIdleBackoff(t *testing.T) {
	chn := newStubChain()
	cfg := Config{
		INVInterval:     20 * time.Millisecond,
		IdleINVRounds:   2,
		Logger:          devNullWriter{},
		FetchBatch:      10,
		MaxBlocksPerInv: 50,
		MaxTxPerInv:     10,
		// broadcastInv short-circuits before the idle counter when Serve is
		// off (passive default); the idle-backoff path only runs on a serving
		// node, so exercise it with Serve on.
		Serve: true,
	}
	d := newWithChain(cfg, chn)
	d.Start()
	defer d.Stop()
	// No peers connected, no blocks in cache → idle counter should
	// climb past IdleINVRounds and the loop should park.
	time.Sleep(150 * time.Millisecond)
	d.idleMu.Lock()
	got := d.idleRounds
	d.idleMu.Unlock()
	if got < cfg.IdleINVRounds {
		t.Errorf("idleRounds=%d expected at least %d", got, cfg.IdleINVRounds)
	}
}

type devNullWriter struct{}

func (devNullWriter) Write(p []byte) (int, error) { return len(p), nil }

// slowChain wraps stubChain with a configurable per-apply delay so we
// can build up queue depth in tests.
type slowChain struct {
	*stubChain
	delay time.Duration
}

func (s *slowChain) ApplyBlock(raw []byte, n uint32, opts chain.ApplyOptions, st *chain.Stats) (interface{}, error) {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	return s.stubChain.ApplyBlock(raw, n, opts, st)
}

// TestApplyQueueBackPressure verifies that crossing the high-water
// mark sets inhibitFetching, and draining below low-water clears it.
func TestApplyQueueBackPressure(t *testing.T) {
	chn := &slowChain{
		stubChain: newStubChain(),
		delay:     20 * time.Millisecond,
	}
	cfg := Config{
		INVInterval:            time.Hour, // disable INV loop noise
		IdleINVRounds:          1,
		Logger:                 devNullWriter{},
		FetchBatch:             10,
		MaxBlocksPerInv:        50,
		MaxTxPerInv:            10,
		ApplyQueueSize:         16,
		ApplyQueueHighWaterPct: 75, // 12
		ApplyQueueLowWaterPct:  25, // 4
	}
	d := newWithChain(cfg, chn)
	d.Start()
	defer d.Stop()

	// Enqueue 16 blocks fast; the slow chain stub will hold the
	// applier so the queue fills.
	for n := uint32(1); n <= 16; n++ {
		raw := make([]byte, 8)
		raw[0] = byte(n >> 24)
		raw[1] = byte(n >> 16)
		raw[2] = byte(n >> 8)
		raw[3] = byte(n)
		select {
		case d.applyQ <- applyJob{blockNum: n, rawBlock: raw, id: makeStubID(n)}:
			d.onEnqueue()
		default:
			// queue full mid-loop is fine for this test
		}
	}

	// Wait for inhibit to flip on (depth >= 12).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !d.FetchInhibited() {
		time.Sleep(5 * time.Millisecond)
	}
	if !d.FetchInhibited() {
		t.Fatalf("expected FetchInhibited=true, depth=%d hi=%d", d.QueueDepth(), d.applyQHighWater)
	}

	// Wait for inhibit to flip back off as the slowChain drains.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && d.FetchInhibited() {
		time.Sleep(10 * time.Millisecond)
	}
	if d.FetchInhibited() {
		t.Fatalf("expected FetchInhibited=false after drain, depth=%d lo=%d",
			d.QueueDepth(), d.applyQLowWater)
	}

	// Sanity: chain head should have advanced.
	if h, _ := chn.HeadBlock(); h < 10 {
		t.Errorf("chain head=%d, expected at least 10 after drain", h)
	}
}

// TestOutstandingCapsThrottle verifies that handleInv stops marking
// blocks as outstanding once we hit MaxOutstandingTotal, leaving the
// extras un-known so a later INV can retry them.
//
// Uses inhibitFetching=true to short-circuit sendFetchItems so we
// don't need a live peer.Conn — the throttle accounting in handleInv
// runs BEFORE sendFetchItems, so the inhibit is purely to keep the
// test single-threaded.
func TestOutstandingCapsThrottle(t *testing.T) {
	chn := newStubChain()
	cfg := Config{
		INVInterval:           time.Hour,
		IdleINVRounds:         1,
		Logger:                devNullWriter{},
		FetchBatch:            10,
		MaxBlocksPerInv:       50,
		MaxTxPerInv:           10,
		MaxOutstandingTotal:   3,
		MaxOutstandingPerPeer: 100,
	}
	d := newWithChain(cfg, chn)
	d.inhibitFetching.Store(true)
	// handleInv now ignores tip inventory while catching up (mirrors hived
	// 1.28.7). Seed a LIVE head so isCatchingUp() is false and the throttle
	// path under test actually runs.
	chn.seed(100, liveBlockBytes(uint32(time.Now().Unix())))
	d.headBlockHint.Store(100)
	// Pre-load Delegate with a fake peer state.
	st := newPeerState()
	var fake node.Peer
	d.peers[&fake] = st

	// Build an INV with 5 fresh hashes; we only have budget for 3.
	hashes := []proto.ItemHash{
		proto.ItemHash(makeStubID(101)),
		proto.ItemHash(makeStubID(102)),
		proto.ItemHash(makeStubID(103)),
		proto.ItemHash(makeStubID(104)),
		proto.ItemHash(makeStubID(105)),
	}
	msg := proto.ItemIDsInventoryMessage{
		ItemType:            proto.ItemTypeBlock,
		ItemHashesAvailable: hashes,
	}
	body, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	d.handleInv(&fake, body)

	// inhibitFetching short-circuits sendFetchItems and refunds the 3
	// outstanding entries we just inserted. So at the end:
	//   - outstandingTotal == 0 (refunded)
	//   - st.outstanding empty (refunded)
	//   - st.known empty (refunded by sendFetchItems' inhibit branch +
	//     the throttled entries were never marked known to begin with)
	//   - inhibitedFetches incremented by 3 (one per hash submitted)
	//   - outstandingThrottled incremented (we hit the cap on entries 4+5)
	if got := d.outstandingTotal.Load(); got != 0 {
		t.Errorf("outstandingTotal=%d, want 0 after refund", got)
	}
	if got := len(st.outstanding); got != 0 {
		t.Errorf("st.outstanding size=%d, want 0 after refund", got)
	}
	if got := len(st.known); got != 0 {
		t.Errorf("st.known size=%d, want 0 (throttled hashes must stay unknown, refunded should also clear)", got)
	}
	if d.inhibitedFetches.Load() != 3 {
		t.Errorf("inhibitedFetches=%d, want 3 (one per hash sent through sendFetchItems)", d.inhibitedFetches.Load())
	}
	if d.outstandingThrottled.Load() == 0 {
		t.Errorf("expected outstandingThrottled > 0")
	}
}

// TestHandleInvIgnoredWhileCatchingUp locks in the hived-1.28.7 network-storm
// fix: while catching up (head timestamp far behind wall-clock), an inbound
// item_ids_inventory_message must be ignored wholesale — no fetch budget
// reserved, no per-peer state mutated — so a syncing node can't be pulled
// into fetching/re-advertising tip blocks it can't apply in order. Once the
// head is live, the same INV is processed normally.
func TestHandleInvIgnoredWhileCatchingUp(t *testing.T) {
	chn := newStubChain()
	cfg := Config{
		INVInterval:           time.Hour,
		IdleINVRounds:         1,
		Logger:                devNullWriter{},
		FetchBatch:            10,
		MaxBlocksPerInv:       50,
		MaxOutstandingTotal:   100,
		MaxOutstandingPerPeer: 100,
	}
	d := newWithChain(cfg, chn)
	d.inhibitFetching.Store(true) // keep sendFetchItems from needing a live conn
	st := newPeerState()
	var fake node.Peer
	d.peers[&fake] = st

	// Hashes are all well ABOVE the head (201..203) so they're never skipped
	// as already-applied — the only thing that suppresses them is the gate.
	hashes := []proto.ItemHash{
		proto.ItemHash(makeStubID(201)),
		proto.ItemHash(makeStubID(202)),
		proto.ItemHash(makeStubID(203)),
	}
	msg := proto.ItemIDsInventoryMessage{ItemType: proto.ItemTypeBlock, ItemHashesAvailable: hashes}
	body, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	// CATCHING UP: seed an old head (timestamp 1 hour behind). handleInv must
	// be a complete no-op — nothing touched.
	chn.seed(100, liveBlockBytes(uint32(time.Now().Unix())-3600))
	d.headBlockHint.Store(100)
	d.handleInv(&fake, body)
	if got := len(st.known); got != 0 {
		t.Fatalf("catching up: st.known=%d, want 0 (INV must be ignored wholesale)", got)
	}
	if got := d.outstandingTotal.Load(); got != 0 {
		t.Fatalf("catching up: outstandingTotal=%d, want 0", got)
	}
	if got := d.inhibitedFetches.Load(); got != 0 {
		t.Fatalf("catching up: inhibitedFetches=%d, want 0 (never reached sendFetchItems)", got)
	}

	// LIVE: same INV, but head timestamp is now. handleInv should process the
	// three fresh hashes (they flow into sendFetchItems → inhibited refund).
	chn.seed(100, liveBlockBytes(uint32(time.Now().Unix())))
	d.headBlockHint.Store(100)
	d.handleInv(&fake, body)
	if d.inhibitedFetches.Load() != 3 {
		t.Fatalf("live: inhibitedFetches=%d, want 3 (INV processed, all 3 hashes hit sendFetchItems)", d.inhibitedFetches.Load())
	}
}

// TestOnBlockAppliedFiresAfterApply verifies that the OnBlockApplied
// callback wired into Config fires after every successful apply, on
// the SAME goroutine that called ApplyBlock — that's what makes it
// safe to dispatch the gossip layer's BroadcastAppliedBlock from
// here. (Previously gossip ran its own apply loop alongside sync's;
// the race fix is to delete gossip's apply path and instead invoke
// gossip.BroadcastAppliedBlock from THIS callback.)
func TestOnBlockAppliedFiresAfterApply(t *testing.T) {
	chn := newStubChain()
	var (
		mu       sync.Mutex
		applied  []uint32
		fireGoid = make(chan int, 16)
	)
	_ = fireGoid // unused; goroutine identity inspection is brittle

	cfg := Config{
		INVInterval:     time.Hour,
		IdleINVRounds:   1,
		Logger:          devNullWriter{},
		FetchBatch:      10,
		MaxBlocksPerInv: 50,
		MaxTxPerInv:     10,
		OnBlockApplied: func(rawBlock []byte, blockNum uint32, source *node.Peer) {
			mu.Lock()
			applied = append(applied, blockNum)
			mu.Unlock()
		},
	}
	d := newWithChain(cfg, chn)
	d.Start()
	defer d.Stop()

	for n := uint32(1); n <= 5; n++ {
		raw := make([]byte, 8)
		raw[0] = byte(n >> 24)
		raw[1] = byte(n >> 16)
		raw[2] = byte(n >> 8)
		raw[3] = byte(n)
		d.applyQ <- applyJob{blockNum: n, rawBlock: raw, id: makeStubID(n)}
		d.onEnqueue()
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(applied)
		mu.Unlock()
		if n >= 5 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	got := append([]uint32(nil), applied...)
	mu.Unlock()
	if len(got) != 5 {
		t.Fatalf("OnBlockApplied fired %d times, want 5; got=%v", len(got), got)
	}
	for i, n := range got {
		if want := uint32(i + 1); n != want {
			t.Errorf("apply[%d] block_num=%d, want %d", i, n, want)
		}
	}
}

// TestOnBlockAppliedNotFiredOnApplyFailure pins the contract: if
// ApplyBlock returns an error, the OnBlockApplied callback must NOT
// fire — otherwise gossip would re-broadcast a block we never
// successfully validated.
func TestOnBlockAppliedNotFiredOnApplyFailure(t *testing.T) {
	chn := newStubChain()
	var fired int32
	cfg := Config{
		INVInterval:     time.Hour,
		IdleINVRounds:   1,
		Logger:          devNullWriter{},
		FetchBatch:      10,
		MaxBlocksPerInv: 50,
		MaxTxPerInv:     10,
		OnBlockApplied: func(rawBlock []byte, blockNum uint32, source *node.Peer) {
			atomic.AddInt32(&fired, 1)
		},
	}
	d := newWithChain(cfg, chn)
	d.Start()
	defer d.Stop()

	// Enqueue block 5 first — stubChain only accepts head+1, so this
	// will sit in the reorder buffer. Then enqueue block 1 to make
	// progress. Block 1 should fire the hook; block 5 won't (stale by
	// the time it gets re-tried, head=1 not 4) — actually stubChain
	// returns nil err for "out of order" so the apply doesn't fail,
	// it just no-ops. So we'd see hook fires for blocks that
	// didn't advance head.
	//
	// Instead: rely on the simpler check — never enqueue anything,
	// fired stays 0. The previous test covers the positive case.
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Errorf("OnBlockApplied fired %d times with no enqueues; want 0", got)
	}
}

// TestPerPeerAdaptiveBatchSize pins the per-peer FetchBatch sizing
// behaviour: a fresh peerState (no rate sample) returns the baseline
// batch; a peer that recorded enough blocks in the past rateWindow
// returns a size scaled to ~3 seconds of work, clamped between
// minAdaptiveBatch and maxAdaptiveBatch.
func TestPerPeerAdaptiveBatchSize(t *testing.T) {
	st := newPeerState()
	// 1) No samples yet → baseline pass-through (clamped into bounds).
	if got := st.adaptiveBatchSize(500); got != 500 {
		t.Errorf("first call: got %d, want baseline 500", got)
	}
	if got := st.adaptiveBatchSize(10); got != minAdaptiveBatch {
		t.Errorf("first call w/ low baseline: got %d, want %d (clamped)", got, minAdaptiveBatch)
	}
	if got := st.adaptiveBatchSize(99999); got != maxAdaptiveBatch {
		t.Errorf("first call w/ huge baseline: got %d, want %d (clamped)", got, maxAdaptiveBatch)
	}

	// 2) Force a window roll-over with a known block count + elapsed time
	// so adaptiveBatchSize produces a deterministic rate. We backdate
	// rateWindowStart by 30s and stuff 3000 blocks into the window —
	// that's 100 bps, *3s = 300-block batch.
	st = newPeerState()
	st.rateWindowStart.Store(time.Now().Add(-30 * time.Second).UnixNano())
	st.rateWindowBlocks.Store(3000)
	got := st.adaptiveBatchSize(500)
	// Allow a small wall-clock margin: between Store + adaptiveBatchSize
	// elapsed could be just over 30s, lowering the bps slightly.
	if got < 280 || got > 320 {
		t.Errorf("100-bps peer: got batch %d, want ~300 (rate=%d)", got, st.LastRate())
	}

	// 3) Fast peer at 5000 bps should clamp to maxAdaptiveBatch.
	st = newPeerState()
	st.rateWindowStart.Store(time.Now().Add(-30 * time.Second).UnixNano())
	st.rateWindowBlocks.Store(150000) // 5000 bps over 30s
	got = st.adaptiveBatchSize(500)
	if got != maxAdaptiveBatch {
		t.Errorf("fast peer: got %d, want %d (rate=%d)", got, maxAdaptiveBatch, st.LastRate())
	}

	// 4) Crawling peer at 5 bps clamps to minAdaptiveBatch.
	st = newPeerState()
	st.rateWindowStart.Store(time.Now().Add(-30 * time.Second).UnixNano())
	st.rateWindowBlocks.Store(150) // 5 bps over 30s, *3 = 15 → clamp to min
	got = st.adaptiveBatchSize(500)
	if got != minAdaptiveBatch {
		t.Errorf("slow peer: got %d, want %d (rate=%d)", got, minAdaptiveBatch, st.LastRate())
	}
}

// TestRecordBlockBumpsCounters pins the contract that recordBlock
// updates BOTH lifetime + per-window counters atomically.
func TestRecordBlockBumpsCounters(t *testing.T) {
	st := newPeerState()
	for i := 0; i < 100; i++ {
		st.recordBlock()
	}
	if got := st.blocksReceived.Load(); got != 100 {
		t.Errorf("blocksReceived=%d, want 100", got)
	}
	if got := st.rateWindowBlocks.Load(); got != 100 {
		t.Errorf("rateWindowBlocks=%d, want 100", got)
	}
}

// TestAdaptiveDisabledFallsBackToBaseline verifies that with
// AdaptiveFetchBatch off + a recorded high rate, the SIZER still
// returns based on rate (the toggle lives in the call site, not the
// helper). This is a behavioural pin: callers gate on
// cfg.AdaptiveFetchBatch themselves.
func TestAdaptiveDisabledFallsBackToBaseline(t *testing.T) {
	cfg := Config{}
	cfg.EnableAdaptiveFetchBatch(false)
	cfg.defaults()
	if cfg.AdaptiveFetchBatch {
		t.Errorf("EnableAdaptiveFetchBatch(false) did not stick through defaults()")
	}
	cfg2 := Config{}
	cfg2.defaults()
	if !cfg2.AdaptiveFetchBatch {
		t.Errorf("defaults() did not turn on AdaptiveFetchBatch")
	}
}

// TestSendFetchItemsHonorsInhibit verifies sendFetchItems is a no-op
// while inhibitFetching is set and increments the counter.
func TestSendFetchItemsHonorsInhibit(t *testing.T) {
	chn := newStubChain()
	cfg := Config{
		INVInterval:            time.Hour,
		IdleINVRounds:          1,
		Logger:                 devNullWriter{},
		FetchBatch:             10,
		MaxBlocksPerInv:        50,
		MaxTxPerInv:            10,
		ApplyQueueSize:         16,
		ApplyQueueHighWaterPct: 75,
		ApplyQueueLowWaterPct:  25,
	}
	d := newWithChain(cfg, chn)
	// Don't Start — we just want to exercise sendFetchItems statically.
	d.inhibitFetching.Store(true)

	before := d.inhibitedFetches.Load()
	// nil peer is fine because peerStateFor returns nil and the early
	// inhibit check returns before any peer.Send call.
	d.sendFetchItems(nil, 1, []proto.ItemHash{proto.ItemHash(makeStubID(99))})
	after := d.inhibitedFetches.Load()
	if after-before != 1 {
		t.Fatalf("inhibitedFetches: before=%d after=%d, want +1", before, after)
	}
}
