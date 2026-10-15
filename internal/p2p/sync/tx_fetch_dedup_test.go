package sync

import (
	"testing"
	"time"

	"gopherd/internal/crypto"
	"gopherd/internal/p2p/node"
	"gopherd/internal/protocol/block"
)

type countingMempool struct{ n int }

func (m *countingMempool) AddPendingRaw([]byte) error { m.n++; return nil }

// TestTxFetchedFromOnePeer pins that a transaction advertised by several peers
// is fetched from one of them, as hived does, and that its body is not counted
// as unrequested even after pruneLoop has forgotten it in `known`.
//
// Before the global table every advertising peer was asked: 3.5M fetches for
// 220k unique admissions on mainnet, the extra copies all refused.
func TestTxFetchedFromOnePeer(t *testing.T) {
	mp := &countingMempool{}
	d := newWithChain(Config{Logger: devNullWriter{}, Mempool: mp}, newStubChain())
	body := []byte("a signed transaction")
	id := block.ID(crypto.RIPEMD160(body))
	now := time.Now().UnixNano()
	peers := make([]*node.Peer, 4)
	asked := 0
	for i := range peers {
		peers[i] = new(node.Peer)
		st := newPeerState()
		d.peers[peers[i]] = st
		if i < 3 && d.claimTxFetch(st, peers[i], id, now) {
			asked++
		}
	}
	if asked != 1 {
		t.Fatalf("fetched from %d of 3 advertising peers, want 1", asked)
	}
	// pruneLoop drops transaction ids from `known` (they fail its height
	// test); the answer must still count as requested.
	delete(d.peers[peers[0]].known, id)
	d.OnTransactionReceived(peers[0], body)
	if got := d.txUnrequested.Load(); got != 0 {
		t.Fatalf("unrequested=%d for a body we asked for", got)
	}
	if mp.n != 1 {
		t.Fatalf("mempool saw %d copies, want 1", mp.n)
	}
	// Once delivered, a later advert, past the stale window, is not fetched.
	if d.claimTxFetch(d.peers[peers[3]], peers[3], id, now+int64(time.Minute)) {
		t.Fatal("fetched a transaction we already have")
	}
}

// TestTxFetchReassignsStaleClaim pins the fallback: a peer that does not
// deliver within reRequestStale loses the claim to the next advertiser.
func TestTxFetchReassignsStaleClaim(t *testing.T) {
	d := newWithChain(Config{Logger: devNullWriter{}}, newStubChain())
	a, b := new(node.Peer), new(node.Peer)
	id := block.ID{0xfe, 1}
	now := time.Now().UnixNano()
	if !d.tryReserveTxFetch(id, a, now) {
		t.Fatal("first claim refused")
	}
	if d.tryReserveTxFetch(id, b, now+int64(time.Second)) {
		t.Fatal("second peer claimed a fresh reservation")
	}
	if !d.tryReserveTxFetch(id, b, now+int64(reRequestStale)) {
		t.Fatal("stale claim not handed to the next advertiser")
	}
	d.pruneTxFetch(now + int64(txFetchTTL) + int64(time.Minute))
	if len(d.txFetch) != 0 {
		t.Fatal("prune kept an expired entry")
	}
}
