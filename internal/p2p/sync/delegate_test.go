package sync

import (
	"sync"
	"testing"
	"time"

	"hived-go/internal/chain"
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
