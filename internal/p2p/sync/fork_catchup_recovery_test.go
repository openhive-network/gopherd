package sync

import (
	"testing"
	"time"

	"gopherd/internal/p2p/node"
	"gopherd/internal/p2p/proto"
)

// rivalStubID builds an ID at the SAME height as makeStubID(num) but with a
// different body — a competing block for that slot, exactly like the two
// blocks mainnet had at 109,424,068 on 2026-08-28 (threespeak's, which the
// network kept, and seattlea's, which this node applied).
func rivalStubID(num uint32) proto.ItemHash {
	id := makeStubID(num)
	id[4], id[5] = 0xfe, 0xed
	return proto.ItemHash(id)
}

// TestCatchupFetchesRivalHeadBlockWhileCatchingUp is the regression test for
// the 2026-08-28 live wedge: gopherd applied the losing block at height
// 109,424,068, so its head stopped advancing, so head age crossed
// liveAdvertiseWindowSec, so isCatchingUp() latched TRUE — and handleInv (the
// tip-inventory path) ignores adverts wholesale while catching up. The node
// then sat 33 hours at a dead fork having applied ZERO blocks, with ~4000
// fetches perpetually in flight and everything it received orphaning into a
// forkdb that grew 66 -> 8440.
//
// The deadly part is the feedback loop: being on a wrong fork is what makes
// you fall behind, and falling behind is what disables the tip-inventory path.
// So the CATCH-UP path is the only remaining route back, and it must keep
// offering the rival block at our own head height to fork choice. If this path
// ever picks up the linear "skip anything <= head" rule, a depth-1 fork
// becomes permanent and no amount of reconnecting recovers it.
//
// Note this is a fetch-DECISION test, not a fork-choice test: reorg mechanics
// live in chain (TestPushLiveBlockReorg). What is asserted here is the link
// that broke — that the block fork choice needs is actually requested.
func TestCatchupFetchesRivalHeadBlockWhileCatchingUp(t *testing.T) {
	const headNum = uint32(109424068)

	newDelegate := func(forkAware bool) (*Delegate, *peerState, *node.Peer) {
		chn := newStubChain()
		d := newWithChain(Config{
			INVInterval:           time.Hour,
			IdleINVRounds:         1,
			Logger:                devNullWriter{},
			FetchBatch:            10,
			MaxBlocksPerInv:       50,
			MaxOutstandingTotal:   100,
			MaxOutstandingPerPeer: 100,
			ForkAware:             forkAware,
		}, chn)
		// Keeps sendFetchItems from needing a live connection while still
		// counting every hash that reached it.
		d.inhibitFetching.Store(true)
		st := newPeerState()
		fake := new(node.Peer)
		d.peers[fake] = st
		// Head is an HOUR stale: isCatchingUp() is TRUE, which is precisely
		// the state the live node was stuck in.
		chn.seed(headNum, liveBlockBytes(uint32(time.Now().Unix())-3600))
		d.headBlockHint.Store(headNum)
		if !d.isCatchingUp() {
			t.Fatal("setup: expected isCatchingUp() true with an hour-stale head")
		}
		return d, st, fake
	}

	// The peer answers our fetch_blockchain_item_ids with the fork point's
	// successor — the RIVAL block at our head height — followed by its child.
	// This is what hived serves once it resolves the fork point from our
	// synopsis, and it is the only way we ever learn the rival branch exists.
	// Offer the rival ALONE. Under inhibitFetching the handler refunds
	// st.outstanding and the global inflight reservation, so a count is the
	// only signal that survives the call — isolating the rival is what makes
	// that count identity-precise rather than merely "something was fetched".
	marshal := func(hashes ...proto.ItemHash) []byte {
		m := proto.BlockchainItemIDsInventoryMessage{
			ItemType:            proto.ItemTypeBlock,
			ItemHashesAvailable: hashes,
		}
		body, err := m.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	rivalOnly := marshal(rivalStubID(headNum))
	rivalAndChild := marshal(rivalStubID(headNum), proto.ItemHash(makeStubID(headNum+1)))

	t.Run("fork-aware fetches the rival at head height", func(t *testing.T) {
		d, _, fake := newDelegate(true)
		d.handleBlockchainItemIDsInventory(fake, rivalOnly)

		// The catching-up gate belongs to handleInv ONLY. If it ever spreads
		// to this handler the node can never leave a dead fork.
		if got := d.invIgnoredCatchup.Load(); got != 0 {
			t.Errorf("invIgnoredCatchup=%d, want 0 (the catch-up path must never be gated by isCatchingUp)", got)
		}
		if got := d.inhibitedFetches.Load(); got != 1 {
			t.Errorf("inhibitedFetches=%d, want 1: the rival block at our own head height was NOT requested, so fork choice can never see the competing branch and a depth-1 fork becomes permanent", got)
		}

		// And the rival does not crowd out ordinary forward progress.
		d2, _, fake2 := newDelegate(true)
		d2.handleBlockchainItemIDsInventory(fake2, rivalAndChild)
		if got := d2.inhibitedFetches.Load(); got != 2 {
			t.Errorf("inhibitedFetches=%d, want 2 (rival-at-head AND its child)", got)
		}
	})

	t.Run("linear mode still skips it", func(t *testing.T) {
		d, _, fake := newDelegate(false)
		d.handleBlockchainItemIDsInventory(fake, rivalOnly)

		// A strictly-linear node can only apply head+1, so the rival is
		// genuinely useless to it. This half keeps the fork-aware assertion
		// above honest: it proves the test discriminates on ForkAware rather
		// than passing because everything is fetched unconditionally.
		if got := d.inhibitedFetches.Load(); got != 0 {
			t.Errorf("inhibitedFetches=%d, want 0 (linear mode can only ever apply head+1)", got)
		}

		d2, _, fake2 := newDelegate(false)
		d2.handleBlockchainItemIDsInventory(fake2, rivalAndChild)
		if got := d2.inhibitedFetches.Load(); got != 1 {
			t.Errorf("inhibitedFetches=%d, want 1 (only the head+1 child)", got)
		}
	})
}
