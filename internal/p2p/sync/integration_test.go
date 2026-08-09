package sync

import (
	"net"
	"testing"
	"time"

	"hived-go/internal/p2p/node"
)

// TestLateJoinerCatchesUp verifies the end-to-end catch-up flow:
//
//  1. Node A starts with blocks 1..10 in its recent-block cache
//     (simulating a witness that's been producing for a while).
//  2. Node B starts FRESH with head=0 and dials A.
//  3. After the connection-handshake completes, B's sync delegate
//     should issue fetch_blockchain_item_ids, A should reply with
//     the forward block list, B should issue fetch_items, A should
//     reply with the raw blocks, and B should apply them in order.
//
// Since the blocks A holds are SYNTHETIC (built only to round-trip
// the sync wire format), B's stub ApplyBlock just records them in
// arrival order — we don't run the real evaluator pipeline here.
// The protocol-level state machine is what's under test.
func TestLateJoinerCatchesUp(t *testing.T) {
	// --- Set up node A: holder of blocks 1..10. ---
	chainA := newStubChain()
	for n := uint32(1); n <= 10; n++ {
		// Synthetic raw block: [block_num as 4 BE bytes][padding to 80 bytes]
		// — enough that BlockMessage's len-vs-id check passes.
		raw := make([]byte, 80)
		raw[0] = byte(n >> 24)
		raw[1] = byte(n >> 16)
		raw[2] = byte(n >> 8)
		raw[3] = byte(n)
		chainA.seed(n, raw)
	}

	// Pre-bind to discover an addr.
	rawL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := rawL.Addr().String()
	rawL.Close()

	syncA := newWithChain(Config{
		INVInterval:     50 * time.Millisecond,
		FetchBatch:      10,
		MaxBlocksPerInv: 50,
		MaxTxPerInv:     10,
		IdleINVRounds:   100, // never sleep during the test
		Logger:          devNullWriter{},
		Serve:           true, // A is the block source; it must answer fetches
	}, chainA)
	multiA := node.NewMultiDelegate(syncA)
	nodeA := node.New(node.Config{
		Endpoint:  addr,
		UserAgent: "ghive-sync-A",
		Logger:    devNullWriter{},
	}, nilChain{}, multiA)
	if err := nodeA.Start(); err != nil {
		t.Fatal(err)
	}
	defer nodeA.Stop()
	syncA.Start()
	defer syncA.Stop()

	// Give A a moment to bind.
	time.Sleep(50 * time.Millisecond)

	// --- Set up node B: empty chain, dials A. ---
	chainB := newStubChain()
	syncB := newWithChain(Config{
		INVInterval:     50 * time.Millisecond,
		FetchBatch:      10,
		MaxBlocksPerInv: 50,
		MaxTxPerInv:     10,
		IdleINVRounds:   100,
		Logger:          devNullWriter{},
	}, chainB)
	multiB := node.NewMultiDelegate(syncB)
	nodeB := node.New(node.Config{
		SeedNodes: []string{addr},
		UserAgent: "ghive-sync-B",
		Logger:    devNullWriter{},
	}, nilChain{}, multiB)
	if err := nodeB.Start(); err != nil {
		t.Fatal(err)
	}
	defer nodeB.Stop()
	syncB.Start()
	defer syncB.Stop()

	// Wait for catch-up. B should reach head=10.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		head, _ := chainB.HeadBlock()
		if head >= 10 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	headB, _ := chainB.HeadBlock()
	if headB < 10 {
		t.Fatalf("node B did not catch up: head=%d want >=10", headB)
	}

	// Sanity: B should have applied blocks in order with the right
	// raw bytes (the synthetic prefix encodes the block num).
	for n := uint32(1); n <= 10; n++ {
		raw := chainB.RecentBlockBytes(n)
		if raw == nil {
			t.Errorf("block %d missing on node B", n)
			continue
		}
		if len(raw) < 4 {
			t.Errorf("block %d on B is %d bytes, want >=4", n, len(raw))
			continue
		}
		gotNum := uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3])
		if gotNum != n {
			t.Errorf("block %d on B carries num %d in payload", n, gotNum)
		}
	}
}

// syntheticBlock builds an 80-byte round-trip block whose payload encodes
// `n` in the first 4 BE bytes and whose timestamp field (offset 20, LE — see
// blockTimestamp) is `ts`. A live `ts` makes a node holding it read as
// in-sync (isCatchingUp() == false).
func syntheticBlock(n, ts uint32) []byte {
	raw := make([]byte, 80)
	raw[0] = byte(n >> 24)
	raw[1] = byte(n >> 16)
	raw[2] = byte(n >> 8)
	raw[3] = byte(n)
	raw[20] = byte(ts)
	raw[21] = byte(ts >> 8)
	raw[22] = byte(ts >> 16)
	raw[23] = byte(ts >> 24)
	return raw
}

// TestINVPropagation verifies the steady-state (live-tip) INV path: an
// IN-SYNC node B receives A's item_ids_inventory advertisement for a newly
// produced tip block and fetches it. B must be live (recent head timestamp)
// for this to fire — a catching-up node deliberately ignores tip inventory
// (hived 1.28.7 network-storm fix, see handleInv), so B is pre-seeded at the
// live tip and only the final block arrives via INV. (Cousin of
// TestLateJoinerCatchesUp, which covers the initial-sync
// fetch_blockchain_item_ids handshake for a from-scratch node.)
func TestINVPropagation(t *testing.T) {
	chainA := newStubChain()
	chainB := newStubChain()
	now := uint32(time.Now().Unix())
	// B starts IN SYNC: it already holds live blocks 1..4 (head=4, live ts),
	// so isCatchingUp() is false and handleInv will act on A's advert.
	for n := uint32(1); n <= 4; n++ {
		chainB.seed(n, syntheticBlock(n, now))
	}

	rawL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := rawL.Addr().String()
	rawL.Close()

	cfg := Config{
		INVInterval:     30 * time.Millisecond,
		FetchBatch:      10,
		MaxBlocksPerInv: 50,
		MaxTxPerInv:     10,
		IdleINVRounds:   100,
		Logger:          devNullWriter{},
		Serve:           true, // A advertises + serves the tip block
	}

	syncA := newWithChain(cfg, chainA)
	multiA := node.NewMultiDelegate(syncA)
	nodeA := node.New(node.Config{
		Endpoint:  addr,
		UserAgent: "ghive-inv-A",
		Logger:    devNullWriter{},
	}, nilChain{}, multiA)
	if err := nodeA.Start(); err != nil {
		t.Fatal(err)
	}
	defer nodeA.Stop()
	syncA.Start()
	defer syncA.Stop()

	time.Sleep(40 * time.Millisecond)

	syncB := newWithChain(cfg, chainB)
	multiB := node.NewMultiDelegate(syncB)
	nodeB := node.New(node.Config{
		SeedNodes: []string{addr},
		UserAgent: "ghive-inv-B",
		Logger:    devNullWriter{},
	}, nilChain{}, multiB)
	if err := nodeB.Start(); err != nil {
		t.Fatal(err)
	}
	defer nodeB.Stop()
	syncB.Start()
	defer syncB.Stop()

	// Wait for the connection.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (nodeA.PeerCount() == 0 || nodeB.PeerCount() == 0) {
		time.Sleep(20 * time.Millisecond)
	}

	// Now A produces the chain up to block 5 (live timestamps) AFTER both
	// peers are connected. B already holds 1..4, so the only block it must
	// learn about via A's INV advert is the new tip, block 5.
	for n := uint32(1); n <= 5; n++ {
		raw := syntheticBlock(n, now)
		chainA.seed(n, raw)
		// Simulate A APPLYING the block: the advert set is event-driven
		// (hived's _new_block_inventory), fed only from the apply path, so
		// seeding the chain alone must not — and does not — advertise anything.
		syncA.queueAdvert(raw, n)
	}
	syncA.NotifyHeadAdvanced()

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		head, _ := chainB.HeadBlock()
		if head >= 5 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	headB, _ := chainB.HeadBlock()
	if headB < 5 {
		t.Fatalf("node B did not receive blocks via INV: head=%d want >=5", headB)
	}
}

// nilChain stub for the node.Node ChainView (it only uses HeadBlock
// for the hello payload).
type nilChain struct{}

func (nilChain) HeadBlock() (uint32, error) { return 0, nil }
