package sync

import (
	"testing"
	"time"
)

// TestAdvertiseNeverOffersStaleBlocks reproduces the mainnet incident: a serving
// node at the live tip advertised the last MaxBlocksPerInv (200) entries of its
// recent ring on every round, filtered only by the per-peer `known` set. A
// freshly-connected peer has an EMPTY known set, so it received 200 block ids
// spanning ~10 minutes of history. The isCatchingUp() gate does not prevent
// this — it tests only the HEAD block's timestamp, not the other 199 entries.
// Nodes older than hived 1.28.7 do not ignore inventory while syncing, so they
// acted on those stale adverts and forked away.
//
// The invariant: a block older than advertiseMaxAgeSec is NEVER offered, no
// matter how fresh our head is or how empty a peer's known set is.
func TestAdvertiseNeverOffersStaleBlocks(t *testing.T) {
	now := time.Now().Unix()
	c := newStubChain()

	// A full ring: 200 blocks at 3s spacing, i.e. ~10 minutes of history,
	// ending at a head produced right now. Exactly the steady state of a
	// healthy serving node at the tip.
	const ring = 200
	base := uint32(108_000_000)
	for i := 0; i < ring; i++ {
		n := base + uint32(i)
		ts := uint32(now - int64(3*(ring-1-i))) // oldest first, head at `now`
		c.seed(n, liveBlockBytes(ts))
	}

	d := newWithChain(Config{Serve: true}, c)
	got := d.selectAdvertIDs(c.RecentBlockNums(), now)

	// Only blocks within advertiseMaxAgeSec (8s ≈ 2-3 blocks at 3s) qualify.
	maxAllowed := advertiseMaxAgeSec/3 + 1
	if len(got) == 0 {
		t.Fatal("advertised nothing at the live tip — the fresh blocks should still be offered")
	}
	if len(got) > maxAllowed {
		t.Fatalf("advertised %d blocks, want <= %d: stale history is being gossiped (this is the incident)", len(got), maxAllowed)
	}
	// Every offered block must be inside the age bound.
	for _, id := range got {
		raw := c.RecentBlockBytesByID(id)
		if raw == nil {
			t.Fatalf("advertised id %x not in the ring", id[:4])
		}
		if age := now - int64(blockTimestamp(raw)); age > advertiseMaxAgeSec {
			t.Fatalf("advertised a block %ds old (limit %ds) — pre-1.28.7 peers fork on this", age, advertiseMaxAgeSec)
		}
	}
}

// TestAdvertiseWatermarkStopsRepeatOffers checks the second guard: once a block
// has been offered it is never offered again, so a peer that connects later
// receives the ongoing new-block stream rather than a replayed backlog.
func TestAdvertiseWatermarkStopsRepeatOffers(t *testing.T) {
	now := time.Now().Unix()
	c := newStubChain()
	base := uint32(108_000_000)
	for i := 0; i < 5; i++ {
		c.seed(base+uint32(i), liveBlockBytes(uint32(now-int64(3*(4-i)))))
	}
	d := newWithChain(Config{Serve: true}, c)

	first := d.selectAdvertIDs(c.RecentBlockNums(), now)
	if len(first) == 0 {
		t.Fatal("first round advertised nothing")
	}
	// Same ring, nothing newly applied: must offer nothing the second time.
	second := d.selectAdvertIDs(c.RecentBlockNums(), now)
	if len(second) != 0 {
		t.Fatalf("re-advertised %d already-offered blocks; a late-joining peer would get a backlog", len(second))
	}

	// One genuinely new block → exactly that block is offered.
	c.seed(base+5, liveBlockBytes(uint32(now)))
	third := d.selectAdvertIDs(c.RecentBlockNums(), now)
	if len(third) != 1 {
		t.Fatalf("after one new block, advertised %d blocks, want exactly 1", len(third))
	}
}

// TestAdvertiseWarmStartOffersNoBacklog covers the startup case the age gate
// exists for: a node warm-started from a snapshot has a ring full of blocks
// that are minutes-to-hours old. Its very first advertise round (watermark
// still unset) must not dump that history at its peers.
func TestAdvertiseWarmStartOffersNoBacklog(t *testing.T) {
	now := time.Now().Unix()
	c := newStubChain()
	base := uint32(108_000_000)
	// Whole ring is an hour old — nothing here is safe to advertise.
	for i := 0; i < 50; i++ {
		c.seed(base+uint32(i), liveBlockBytes(uint32(now-3600)))
	}
	d := newWithChain(Config{Serve: true}, c)

	if got := d.selectAdvertIDs(c.RecentBlockNums(), now); len(got) != 0 {
		t.Fatalf("warm start advertised %d stale blocks, want 0", len(got))
	}
}

// TestAdvertiseGoesSilentWhenBehind covers the CPU-starvation path, which is
// distinct from the new-peer backlog above and needs no new peer at all.
//
// isCatchingUp() — the gate deciding whether we advertise AT ALL — tolerates a
// head up to liveAdvertiseWindowSec (120s) old, i.e. ~40 blocks behind. On a
// contended box we fall behind, still believe we are "live", and keep offering
// a head that is now a minute old to ESTABLISHED peers. Nodes older than hived
// 1.28.7 act on those stale offers and fork away.
//
// The per-block age gate must make this self-correcting: when we cannot keep
// up, nothing qualifies and we go silent until we are back at the tip.
func TestAdvertiseGoesSilentWhenBehind(t *testing.T) {
	now := time.Now().Unix()
	c := newStubChain()
	base := uint32(108_000_000)
	// Newest block is 60s old: we are ~20 blocks behind, but still WELL inside
	// the 120s isCatchingUp window, so the outer gate would let us advertise.
	for i := 0; i < 20; i++ {
		c.seed(base+uint32(i), liveBlockBytes(uint32(now-60-int64(3*(19-i)))))
	}
	d := newWithChain(Config{Serve: true}, c)

	// Precondition: the outer gate really would have permitted advertising —
	// otherwise this test proves nothing about the age gate.
	if d.isCatchingUp() {
		t.Fatal("precondition failed: isCatchingUp() already suppresses this case, test is not exercising the age gate")
	}
	if got := d.selectAdvertIDs(c.RecentBlockNums(), now); len(got) != 0 {
		t.Fatalf("advertised %d blocks while ~20 blocks behind, want 0 (silence) — this forks pre-1.28.7 peers", len(got))
	}
}
