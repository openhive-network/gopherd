package sync

import (
	"testing"
	"time"
)

// The advert set is EVENT-DRIVEN (hived's _new_block_inventory): blocks enter
// it only via queueAdvert from the apply path, and each round drains it whole.
// These tests drive that path — seed the chain so the id is resolvable, then
// queue as the apply loop would.
func advertHarness(t *testing.T) (*stubChain, *Delegate) {
	t.Helper()
	c := newStubChain()
	return c, newWithChain(Config{Serve: true}, c)
}

// TestAdvertiseNeverOffersStaleBlocks is the incident test. Originally the
// advert set was DERIVED by scanning the last MaxBlocksPerInv (200) entries of
// the recent ring, filtered only by the per-peer `known` set — so a newly
// connected peer (empty known set) received 200 ids spanning ~10 minutes of
// history. The isCatchingUp() gate does not prevent that: it tests only the
// HEAD block's timestamp, not the rest of the batch. Nodes older than hived
// 1.28.7 act on stale adverts and fork away.
//
// Now the ring is never consulted, so a full ring of history is simply not
// advertisable — the property holds structurally, not by filtering.
func TestAdvertiseNeverOffersStaleBlocks(t *testing.T) {
	now := time.Now().Unix()
	c, d := advertHarness(t)

	// A full ring: 200 blocks at 3s spacing (~10 min), head produced now.
	const ring = 200
	base := uint32(108_000_000)
	for i := 0; i < ring; i++ {
		c.seed(base+uint32(i), liveBlockBytes(uint32(now-int64(3*(ring-1-i)))))
	}

	// Nothing was applied through us, so nothing may be offered — regardless of
	// how full the ring is or how fresh its head looks.
	if got := d.selectAdvertIDs(now); len(got) != 0 {
		t.Fatalf("advertised %d blocks from the ring alone; the advert set must be event-driven (this is the incident)", len(got))
	}

	// Applying one block queues exactly that block.
	head := base + ring - 1
	d.queueAdvert(liveBlockBytes(uint32(now)), head)
	got := d.selectAdvertIDs(now)
	if len(got) != 1 {
		t.Fatalf("after applying 1 block, advertised %d, want exactly 1", len(got))
	}
	if id, _ := c.RecentBlockIDForNum(head); got[0] != id {
		t.Fatalf("advertised the wrong block: got %x want %x", got[0][:4], id[:4])
	}
}

// TestAdvertiseWatermarkStopsRepeatOffers: a block is offered at most once, so
// a peer that connects later gets the ongoing new-block stream, not a replay.
func TestAdvertiseWatermarkStopsRepeatOffers(t *testing.T) {
	now := time.Now().Unix()
	c, d := advertHarness(t)
	base := uint32(108_000_000)
	c.seed(base, liveBlockBytes(uint32(now)))

	d.queueAdvert(liveBlockBytes(uint32(now)), base)
	if got := d.selectAdvertIDs(now); len(got) != 1 {
		t.Fatalf("first round advertised %d, want 1", len(got))
	}
	// Drained and watermarked: a second round has nothing, and even a duplicate
	// queue of the same block must not re-offer it.
	if got := d.selectAdvertIDs(now); len(got) != 0 {
		t.Fatalf("second round advertised %d, want 0", len(got))
	}
	d.queueAdvert(liveBlockBytes(uint32(now)), base)
	if got := d.selectAdvertIDs(now); len(got) != 0 {
		t.Fatalf("re-queued an already-offered block and it was advertised again (%d)", len(got))
	}
}

// TestAdvertiseWarmStartOffersNoBacklog: a node warm-started from a snapshot
// has a ring full of blocks minutes-to-hours old. Its first advert round must
// not dump that history. With an event-driven set this cannot happen at all.
func TestAdvertiseWarmStartOffersNoBacklog(t *testing.T) {
	now := time.Now().Unix()
	c, d := advertHarness(t)
	base := uint32(108_000_000)
	for i := 0; i < 50; i++ {
		c.seed(base+uint32(i), liveBlockBytes(uint32(now-3600)))
	}
	if got := d.selectAdvertIDs(now); len(got) != 0 {
		t.Fatalf("warm start advertised %d stale blocks, want 0", len(got))
	}
}

// TestAdvertiseGoesSilentWhenBehind covers the CPU-starvation path. isCatchingUp()
// — the gate deciding whether we advertise at all — tolerates a head up to
// liveAdvertiseWindowSec (120s) old, i.e. ~40 blocks behind, so it cannot be
// relied on here. If the advert loop is starved long enough that a queued block
// ages out, it must be dropped rather than shipped.
func TestAdvertiseGoesSilentWhenBehind(t *testing.T) {
	now := time.Now().Unix()
	c, d := advertHarness(t)
	base := uint32(108_000_000)
	c.seed(base, liveBlockBytes(uint32(now-60)))

	// Applied 60s ago, advert loop starved since: still queued, now stale.
	d.queueAdvert(liveBlockBytes(uint32(now-60)), base)

	// Precondition: the outer gate would have permitted this round.
	if d.isCatchingUp() {
		t.Fatal("precondition failed: isCatchingUp() already suppresses this; test is not exercising the age gate")
	}
	if got := d.selectAdvertIDs(now); len(got) != 0 {
		t.Fatalf("advertised %d blocks aged 60s, want 0 (silence) — this forks pre-1.28.7 peers", len(got))
	}
}

// TestAdvertiseNeverOffersBelowPeerLIB pins the age bound to hived's rule
// rather than to a number we chose. Under ONE-BLOCK IRREVERSIBILITY a peer's
// last irreversible block is head-1, so a block two intervals old (6s) is
// already at/below a synced peer's LIB — hived deems it irreversibly on the
// wrong fork and can never accept it. Offering one triggered the 2026-08
// incident, and the damage is not bounded by "we get disconnected": peers relay
// it onward before determining it is bad, so the kick cascades.
func TestAdvertiseNeverOffersBelowPeerLIB(t *testing.T) {
	const blockInterval = 3
	now := time.Now().Unix()
	c, d := advertHarness(t)
	base := uint32(108_000_000)

	c.seed(base, liveBlockBytes(uint32(now-2*blockInterval)))
	d.queueAdvert(liveBlockBytes(uint32(now-2*blockInterval)), base)
	if got := d.selectAdvertIDs(now); len(got) != 0 {
		t.Fatalf("advertised %d blocks at/below a synced peer's LIB (%ds old); hived deems these 'too old'",
			len(got), 2*blockInterval)
	}

	// Sanity: a block within one interval is still offered, so the gate has not
	// regressed into permanent silence.
	c.seed(base+1, liveBlockBytes(uint32(now)))
	d.queueAdvert(liveBlockBytes(uint32(now)), base+1)
	if got := d.selectAdvertIDs(now); len(got) != 1 {
		t.Fatalf("fresh block: advertised %d, want exactly 1", len(got))
	}
}

// TestAdvertiseQueueIsPassiveNoOp: a passive node must not even accumulate an
// advert set, so flipping Serve on later cannot ship a backlog.
func TestAdvertiseQueueIsPassiveNoOp(t *testing.T) {
	now := time.Now().Unix()
	c := newStubChain()
	d := newWithChain(Config{Serve: false}, c)
	base := uint32(108_000_000)
	c.seed(base, liveBlockBytes(uint32(now)))

	d.queueAdvert(liveBlockBytes(uint32(now)), base)
	d.pendingAdvertMu.Lock()
	n := len(d.pendingAdvert)
	d.pendingAdvertMu.Unlock()
	if n != 0 {
		t.Fatalf("passive node queued %d adverts, want 0", n)
	}
}

// TestAdvertiseQueueBounded: if the advert loop is starved the queue must not
// grow without bound.
func TestAdvertiseQueueBounded(t *testing.T) {
	now := time.Now().Unix()
	c, d := advertHarness(t)
	base := uint32(108_000_000)
	for i := 0; i < d.cfg.MaxBlocksPerInv*3; i++ {
		n := base + uint32(i)
		c.seed(n, liveBlockBytes(uint32(now)))
		d.queueAdvert(liveBlockBytes(uint32(now)), n)
	}
	d.pendingAdvertMu.Lock()
	n := len(d.pendingAdvert)
	d.pendingAdvertMu.Unlock()
	if n > d.cfg.MaxBlocksPerInv {
		t.Fatalf("advert queue grew to %d, want <= MaxBlocksPerInv (%d)", n, d.cfg.MaxBlocksPerInv)
	}
}
