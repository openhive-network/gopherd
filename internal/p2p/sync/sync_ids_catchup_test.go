package sync

import (
	"testing"
	"time"

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
	// A nil peer is the assertion: the gate must return BEFORE it reaches
	// p.Send. If the gate is removed this panics rather than silently passing.
	d.handleFetchBlockchainItemIDs(nil, body)
	if got := d.syncIDsRefusedCatchup.Load() - before; got != 1 {
		t.Fatalf("syncIDsRefusedCatchup +%d, want +1 — a node behind the tip must not offer block ids it cannot back", got)
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
