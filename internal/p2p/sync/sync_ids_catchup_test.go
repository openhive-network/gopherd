package sync

import (
	"testing"
	"time"

	"gopherd/internal/p2p/node"
	"gopherd/internal/p2p/proto"
)

// Regression test for the mainnet incident of 2026-09-05: peers dropped this
// node with
//
//	"You are missing a sync item you claim to have, your database is probably
//	 corrupted. Try --rebuild-index."
//
// Mechanism. handleFetchBlockchainItemIDs answers out of the RecentBlocks ring,
// so every id it lists is genuinely held AT THE INSTANT OF THE ANSWER. Holding
// it one network round trip later is a different claim, and the gap between the
// two is set by how fast we are applying:
//
//	at the tip   3 s/block  -> 200-entry ring = ~10 minutes of history
//	catching up  ~70 blk/s  -> 200-entry ring = ~2.9 SECONDS of history
//
// RecentBlockCacheSize is a COUNT, so its retention in seconds collapses by the
// ratio of catch-up rate to tip rate. MaxBlocksPerInv is 200 as well, so the
// list handed out is the whole ring and its OLDEST entry is evicted one applied
// block later (~14 ms). The peer's fetch_items then misses,
// handleFetchItems answers item_not_available, and hived's
// on_item_not_available_message (node.cpp:3159) disconnects us.
//
// hived never hits this because it serves sync items from the block log. Under
// --no-block-log the ring is the ENTIRE serving capacity, so a miss is terminal.
func TestSyncIDsRefusedWhileCatchingUp(t *testing.T) {
	c := newStubChain()
	// A head 10 minutes stale: exactly the shape of a node grinding through
	// catch-up, which is when the ring turns over faster than peers can fetch.
	stale := uint32(time.Now().Unix() - 600)
	const head = 108_000_000
	for n := uint32(head - 199); n <= head; n++ {
		c.seed(n, syntheticBlock(n, stale))
	}
	d := newWithChain(Config{Serve: true, MaxBlocksPerInv: 200}, c)
	d.headBlockHint.Store(head)

	if !d.isCatchingUp() {
		t.Fatal("harness is wrong: a 10-minute-stale head must read as catching up")
	}

	msg := proto.FetchBlockchainItemIDsMessage{
		ItemType:           proto.ItemTypeBlock,
		BlockchainSynopsis: []proto.ItemHash{proto.ItemHash(makeStubID(head - 100))},
	}
	body, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	before := d.syncIDsRefusedCatchup.Load()
	fake, sent := captureReplies(d)
	d.handleFetchBlockchainItemIDs(fake, body)
	if got := d.syncIDsRefusedCatchup.Load() - before; got != 1 {
		t.Fatalf("syncIDsRefusedCatchup +%d, want +1 — a node behind the tip must not offer block ids it cannot back", got)
	}
	// Refusing means an EMPTY inventory, not silence: hived drops a peer that
	// leaves its fetch_blockchain_item_ids unanswered for 6 s.
	assertSingleEmptyInventory(t, *sent)
}

// A passive node (Serve off) must answer too, and with nothing in it. Before
// this, the gate returned without a reply and every hived peer dropped the
// node ~6 s after the handshake (2026-10-28 from-genesis sync: 334 drops in six
// minutes, every connection ~10 s old).
func TestPassiveNodeAnswersSyncIDsWithEmptyInventory(t *testing.T) {
	c := newStubChain()
	now := uint32(time.Now().Unix())
	const head = 108_000_000
	for n := uint32(head - 199); n <= head; n++ {
		c.seed(n, syntheticBlock(n, now))
	}
	d := newWithChain(Config{Serve: false, MaxBlocksPerInv: 200}, c)
	d.headBlockHint.Store(head)

	msg := proto.FetchBlockchainItemIDsMessage{
		ItemType:           proto.ItemTypeBlock,
		BlockchainSynopsis: []proto.ItemHash{proto.ItemHash(makeStubID(head - 100))},
	}
	body, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	fake, sent := captureReplies(d)
	d.handleFetchBlockchainItemIDs(fake, body)
	assertSingleEmptyInventory(t, *sent)
	if got := d.emptySyncIDsReplies.Load(); got != 1 {
		t.Fatalf("emptySyncIDsReplies=%d, want 1", got)
	}
	if got := d.syncIDsRefusedCatchup.Load(); got != 0 {
		t.Fatalf("syncIDsRefusedCatchup=%d, want 0: the passive gate is not the catch-up gate", got)
	}
}

type capturedReply struct {
	typ  proto.MessageType
	body []byte
}

// captureReplies installs the send seam and returns a fake peer plus the list
// of everything the delegate tried to send it.
func captureReplies(d *Delegate) (*node.Peer, *[]capturedReply) {
	fake := new(node.Peer)
	sent := new([]capturedReply)
	d.sendOverride = func(p *node.Peer, typ proto.MessageType, body []byte) error {
		if p != fake {
			panic("reply sent to the wrong peer")
		}
		*sent = append(*sent, capturedReply{typ: typ, body: body})
		return nil
	}
	return fake, sent
}

func assertSingleEmptyInventory(t *testing.T, sent []capturedReply) {
	t.Helper()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want exactly 1 (an empty blockchain_item_ids_inventory)", len(sent))
	}
	if sent[0].typ != proto.BlockchainItemIDsInventoryMessageType {
		t.Fatalf("sent %v, want blockchain_item_ids_inventory", sent[0].typ)
	}
	var reply proto.BlockchainItemIDsInventoryMessage
	if err := reply.Unmarshal(sent[0].body); err != nil {
		t.Fatalf("reply does not decode: %v", err)
	}
	if reply.ItemType != proto.ItemTypeBlock {
		t.Errorf("reply item_type=%d, want %d (echo the request)", reply.ItemType, proto.ItemTypeBlock)
	}
	if len(reply.ItemHashesAvailable) != 0 || reply.TotalRemainingItemCount != 0 {
		t.Fatalf("reply lists %d ids with %d remaining; a gated node must claim NOTHING", len(reply.ItemHashesAvailable), reply.TotalRemainingItemCount)
	}
}

// The gate must be self-clearing: once the head is live, this node is a valid
// sync source again and must answer. A gate that never reopens would silently
// remove us from the set of nodes anyone can sync from — the failure mode is
// invisible locally, which is why it gets its own test.
//
// The serving half of that (an actual id inventory reaching a from-scratch
// peer) is covered end-to-end by TestLateJoinerCatchesUp; here we assert the
// precise condition the serve path is gated on.
func TestSyncIDsServedAtTip(t *testing.T) {
	c := newStubChain()
	now := uint32(time.Now().Unix())
	const head = 108_000_000
	for n := uint32(head - 199); n <= head; n++ {
		c.seed(n, syntheticBlock(n, now))
	}
	d := newWithChain(Config{Serve: true, MaxBlocksPerInv: 200}, c)
	d.headBlockHint.Store(head)

	if d.isCatchingUp() {
		t.Fatal("a live head must not read as catching up — the node would refuse to serve ids forever")
	}
	if got := d.syncIDsRefusedCatchup.Load(); got != 0 {
		t.Fatalf("refusal counter is %d before any request, want 0", got)
	}
}
