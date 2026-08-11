package sync

import (
	"testing"

	"hived-go/internal/protocol/block"
)

// TestTxRelayQueue covers the relay path's queue: bounded, drained in capped
// rounds, and silent on a passive node.
//
// Relay matters beyond citizenship — hived floods every transaction it accepts,
// and a node that only fetches is a pure sink. If every node behaved that way
// transaction propagation would stop.
func TestTxRelayQueue(t *testing.T) {
	_, d := advertHarness(t)       // Serve: true
	d.cfg.RelayTransactions = true // OFF by default until the advert identity is right
	d.cfg.MaxTxPerInv = 4

	for i := 0; i < 10; i++ {
		d.queueTxAdvert(block.ID{byte(i)})
	}
	// Drains at most MaxTxPerInv per round, in order.
	first := d.drainTxAdvert()
	if len(first) != 4 {
		t.Fatalf("drained %d, want 4 (per-round cap)", len(first))
	}
	if first[0] != (block.ID{0}) || first[3] != (block.ID{3}) {
		t.Fatal("drain is not FIFO — oldest transactions should be relayed first")
	}
	second := d.drainTxAdvert()
	if len(second) != 4 || second[0] != (block.ID{4}) {
		t.Fatalf("second round wrong: n=%d head=%v", len(second), second[0])
	}
	// Bounded: a burst must not grow the queue without limit.
	for i := 0; i < 10*d.cfg.MaxTxPerInv; i++ {
		d.queueTxAdvert(block.ID{0xff, byte(i)})
	}
	d.pendingTxAdvertMu.Lock()
	n := len(d.pendingTxAdvert)
	d.pendingTxAdvertMu.Unlock()
	if n > d.cfg.MaxTxPerInv*4 {
		t.Fatalf("relay queue grew to %d, past its %d bound", n, d.cfg.MaxTxPerInv*4)
	}
}

// TestRelayOffByDefault pins that relay does not fire unless explicitly enabled.
// It shipped advertising transactions by the WRONG identity (transaction id
// instead of hived's legacy message hash) and got us dropped by every peer 234
// times, so the default must be off until the identity is verified live.
func TestRelayOffByDefault(t *testing.T) {
	c := newStubChain()
	d := newWithChain(Config{Serve: true}, c) // RelayTransactions unset
	for i := 0; i < 5; i++ {
		d.queueTxAdvert(block.ID{byte(i)})
	}
	if got := d.drainTxAdvert(); len(got) != 0 {
		t.Fatalf("relay queued %d transactions with RelayTransactions unset", len(got))
	}
}

// TestPassiveNodeNeverRelays pins that a fetch-only node advertises nothing.
// A passive node cannot serve, and advertising what you cannot serve is what
// gets a node dropped by its peers.
func TestPassiveNodeNeverRelays(t *testing.T) {
	c := newStubChain()
	d := newWithChain(Config{Serve: false, RelayTransactions: true}, c)
	for i := 0; i < 5; i++ {
		d.queueTxAdvert(block.ID{byte(i)})
	}
	if got := d.drainTxAdvert(); len(got) != 0 {
		t.Fatalf("passive node queued %d transactions for relay; it must advertise nothing", len(got))
	}
}
