package sync

import (
	"testing"
	"time"

	"gopherd/internal/p2p/proto"
)

// One fetch_blockchain_item_ids per peer at a time, as hived does: further
// requests are held until the peer answers or idsRequestTimeout passes.
// Without this the 1 s catch-up kicker stacked 200 KB inventory replies in
// every hived peer's send queue until it passed 1 MiB and hived hung up.
func TestFetchBlockchainItemIDsOneOutstandingPerPeer(t *testing.T) {
	d := newWithChain(Config{}, newStubChain())
	fake, sent := captureReplies(d)
	st := newPeerState()
	d.mu.Lock()
	d.peers[fake] = st
	d.mu.Unlock()

	requests := func() int {
		n := 0
		for _, r := range *sent {
			if r.typ == proto.FetchBlockchainItemIDsMessageType {
				n++
			}
		}
		return n
	}

	// Kicker ticks while the first request is unanswered: one goes out.
	for i := 0; i < 5; i++ {
		d.sendFetchBlockchainItemIDs(fake, [20]byte{})
	}
	if got := requests(); got != 1 {
		t.Fatalf("%d requests outstanding to one peer, want 1", got)
	}
	if held := d.Stats().SyncIDsHeld; held != 4 {
		t.Fatalf("SyncIDsHeld = %d, want 4", held)
	}

	// The peer answers (here with an empty inventory): the next may go.
	empty, err := (&proto.BlockchainItemIDsInventoryMessage{ItemType: proto.ItemTypeBlock}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	d.handleBlockchainItemIDsInventory(fake, empty)
	d.sendFetchBlockchainItemIDs(fake, [20]byte{})
	if got := requests(); got != 2 {
		t.Fatalf("after the reply: %d requests sent, want 2", got)
	}

	// A request the peer never answers stops blocking after the timeout.
	st.idsRequestedAt.Store(time.Now().Add(-idsRequestTimeout - time.Second).UnixNano())
	d.sendFetchBlockchainItemIDs(fake, [20]byte{})
	if got := requests(); got != 3 {
		t.Fatalf("after the timeout: %d requests sent, want 3", got)
	}
}
