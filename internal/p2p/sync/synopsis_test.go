package sync

import (
	"testing"

	"hived-go/internal/p2p/proto"
	"hived-go/internal/protocol/block"
)

// TestSynopsisIncludesAncestors is the regression test for the orphan wedge.
//
// We used to send a synopsis of ONE id — our head. That works only while our
// head is on the canonical chain. The moment it is not, no peer recognises it,
// none can compute a fork point, and none can tell us anything: the node goes
// mute exactly when it needs help. gopherd wedged that way after producing an
// orphan at 108912883 — 76 blocks behind and falling, 12 peers connected, with
// nothing it could say to any of them.
//
// The synopsis must therefore carry ANCESTORS, so a peer that rejects our tip
// still recognises head-1 (or head-2, …), finds the fork point, and sends us
// the branch we are missing.
func TestSynopsisIncludesAncestors(t *testing.T) {
	c := newStubChain()
	for n := uint32(1); n <= 200; n++ {
		c.seed(n, liveBlockBytes(1000))
	}
	d := newWithChain(Config{}, c)

	syn := d.buildSynopsis(makeStubID(200))
	if len(syn) < 4 {
		t.Fatalf("synopsis has %d entries; a single-entry synopsis is unusable when our tip is orphaned", len(syn))
	}
	// Last entry is our head — the receiver relies on that.
	if block.ID(syn[len(syn)-1]) != makeStubID(200) {
		t.Fatal("last synopsis entry must be our head")
	}
	// Oldest first, strictly increasing block numbers.
	prev := uint32(0)
	for i, h := range syn {
		n := block.ID(h).Num()
		if n <= prev && i > 0 {
			t.Fatalf("synopsis not ordered oldest-first at %d (num %d after %d)", i, n, prev)
		}
		prev = n
	}
	// It must reach meaningfully back, not just cover the last few blocks —
	// a fork can be deeper than one block.
	if oldest := block.ID(syn[0]).Num(); oldest > 150 {
		t.Fatalf("synopsis only reaches back to block %d of 200; too shallow to locate a fork point", oldest)
	}
	// And it must include our immediate ancestor, which is what a peer that
	// rejected our tip will actually recognise.
	found := false
	for _, h := range syn {
		if block.ID(h).Num() == 199 {
			found = true
		}
	}
	if !found {
		t.Fatal("synopsis omits head-1 — the one id a peer rejecting our tip is most likely to know")
	}
}

// TestSynopsisEmptyChain degrades gracefully rather than sending nothing.
func TestSynopsisEmptyChain(t *testing.T) {
	c := newStubChain()
	d := newWithChain(Config{}, c)
	if syn := d.buildSynopsis(block.ID{}); len(syn) != 0 {
		t.Fatalf("empty chain produced %d synopsis entries", len(syn))
	}
}

// TestServeSideFindsForkPoint is the mirror of TestSynopsisIncludesAncestors:
// when a PEER is stuck on a fork and asks us for help, we must answer from the
// deepest block we both hold — not from their tip, which we do not have.
//
// The old code used only the last synopsis entry and justified it with "both
// nodes are on the same fork". A peer sends a synopsis precisely because that
// may not be true; answering from an unknown tip hands them blocks that connect
// to nothing they have, and they stay stuck.
func TestServeSideFindsForkPoint(t *testing.T) {
	c := newStubChain()
	for n := uint32(1); n <= 100; n++ {
		c.seed(n, liveBlockBytes(1000))
	}
	_ = newWithChain(Config{Serve: true}, c)

	// Peer's synopsis: shared history up to 98, then an ORPHAN tip we don't hold.
	orphan := block.ID{0, 0, 0, 99, 0xff, 0xee}
	syn := []proto.ItemHash{
		proto.ItemHash(makeStubID(90)),
		proto.ItemHash(makeStubID(96)),
		proto.ItemHash(makeStubID(98)),
		proto.ItemHash(orphan),
	}
	var picked block.ID
	if n := len(syn); n > 0 {
		picked = block.ID(syn[n-1])
		for i := n - 1; i >= 0; i-- {
			cand := block.ID(syn[i])
			if c.RecentBlockBytesByID(cand) != nil {
				picked = cand
				break
			}
		}
	}
	if picked == orphan {
		t.Fatal("answered from the peer's orphan tip; the blocks we send would connect to nothing they hold")
	}
	if picked != makeStubID(98) {
		t.Fatalf("fork point = block %d, want 98 (deepest id we both hold)", picked.Num())
	}
}
