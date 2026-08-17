package witness

import (
	"crypto/sha256"
	"testing"
	"time"

	"gopherd/internal/crypto"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

func dryRunFixture(t *testing.T) (*Plugin, *state.Database, Producer) {
	t.Helper()
	seed := sha256.Sum256([]byte("witness-dry-run"))
	priv, err := crypto.PrivateKeyFromBytes(seed[:])
	if err != nil {
		t.Fatalf("PrivateKeyFromBytes: %v", err)
	}
	cdb, db := newSigningFixture(t, priv)
	prod := Producer{
		Account:     genesis.MainnetInitminer,
		PrivateKey:  priv,
		PublicKey33: priv.PublicKey(),
	}
	p := New(cdb, db, []Producer{prod})
	p.DryRun = true
	// The genesis fixture's head is years old; without this the stale-head gate
	// (correctly) refuses to measure. Tests are exercising the pipeline, not the
	// tip check, which TestDryRunSkipsStaleHead covers directly.
	p.DryRunMaxHeadAgeSec = 1 << 40
	return p, db, prod
}

// TestDryRunLeavesStateUntouched is the safety property the whole feature rests
// on. A locally produced block that got APPLIED would move our head to a block
// no peer has — forking this node off the chain it is tracking. Since the dry
// run is meant to be safe to run against mainnet, head and DGPO time must be
// byte-identical before and after.
func TestDryRunLeavesStateUntouched(t *testing.T) {
	p, db, prod := dryRunFixture(t)

	before, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatalf("GetDGPO: %v", err)
	}

	for i := 0; i < 5; i++ {
		p.dryRunProduce(&prod, before, before.Time+3, prod.Account, true)
	}

	after, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatalf("GetDGPO after: %v", err)
	}
	if after.HeadBlockNumber != before.HeadBlockNumber {
		t.Fatalf("dry run advanced the head: %d -> %d (it must never apply)",
			before.HeadBlockNumber, after.HeadBlockNumber)
	}
	if after.HeadBlockID != before.HeadBlockID {
		t.Fatal("dry run changed head_block_id; it must never apply")
	}
	if after.Time != before.Time {
		t.Fatalf("dry run advanced dgpo time: %d -> %d", before.Time, after.Time)
	}
}

// TestDryRunNeverBroadcasts pins the other half: the block must not reach the
// network. A recording Broadcaster is installed and must stay untouched.
func TestDryRunNeverBroadcasts(t *testing.T) {
	p, db, prod := dryRunFixture(t)
	rec := &recordingPublisher{}
	p.Publisher = rec

	dgpo, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatalf("GetDGPO: %v", err)
	}
	p.dryRunProduce(&prod, dgpo, dgpo.Time+3, prod.Account, true)

	if rec.calls != 0 {
		t.Fatalf("dry run published %d block(s); it must never reach the network", rec.calls)
	}
}

// TestDryRunSelfVerifies checks the pipeline actually produces a block a peer
// would accept the signature on: the recovered signer must be our own key.
// Without this the dry run would report success for blocks nobody could use.
func TestDryRunSelfVerifies(t *testing.T) {
	p, db, prod := dryRunFixture(t)
	dgpo, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatalf("GetDGPO: %v", err)
	}

	const n = 3
	for i := 0; i < n; i++ {
		p.dryRunProduce(&prod, dgpo, dgpo.Time+3, prod.Account, true)
	}

	st := p.DryRunSnapshot()
	if st.Built != n {
		t.Fatalf("built=%d want %d", st.Built, n)
	}
	if st.VerifyOK != n {
		t.Fatalf("verifyOK=%d want %d (verifyFail=%d, errors=%d) — signature does not recover the producer key",
			st.VerifyOK, n, st.VerifyFail, st.Errors)
	}
	if st.Ours != n {
		t.Fatalf("ours=%d want %d", st.Ours, n)
	}
	if st.MaxNs <= 0 {
		t.Fatal("no timing recorded")
	}
	if st.LastBytes <= 0 {
		t.Fatal("no block size recorded")
	}
}

// TestDryRunDetectsWrongKey guards the verifier itself. If a producer's
// declared public key does not match the key it signs with — a plausible
// config error — the dry run must report failure rather than pass silently.
func TestDryRunDetectsWrongKey(t *testing.T) {
	p, db, prod := dryRunFixture(t)

	other := sha256.Sum256([]byte("some-other-key"))
	wrongPriv, err := crypto.PrivateKeyFromBytes(other[:])
	if err != nil {
		t.Fatalf("PrivateKeyFromBytes: %v", err)
	}
	// Signs with prod's key but claims a different pubkey.
	mismatched := Producer{
		Account:     prod.Account,
		PrivateKey:  prod.PrivateKey,
		PublicKey33: wrongPriv.PublicKey(),
	}

	dgpo, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatalf("GetDGPO: %v", err)
	}
	p.dryRunProduce(&mismatched, dgpo, dgpo.Time+3, prod.Account, true)

	st := p.DryRunSnapshot()
	if st.VerifyFail != 1 || st.VerifyOK != 0 {
		t.Fatalf("mismatched key: verifyOK=%d verifyFail=%d, want 0/1 — the verifier is not checking the key",
			st.VerifyOK, st.VerifyFail)
	}
}

// recordingPublisher stands in for the sync delegate. Counting calls is how we
// pin "this block never left the process": publication is the single point
// where a produced block becomes visible to the network AND becomes our head,
// since the witness no longer applies or broadcasts anything itself.
type recordingPublisher struct{ calls int }

func (r *recordingPublisher) PublishBlock(raw []byte, num uint32) error { r.calls++; return nil }

// TestDryRunReportsImpersonation covers the technique of pointing the dry run
// at a REAL witness (e.g. a top-20 account) to exercise its genuine schedule
// slots. We do not hold that witness's signing key, so the block we build would
// be REJECTED by consensus even though it verifies against our own key.
//
// The dry run must say so. A cheerful verify=OK on a block the network would
// throw out is exactly the kind of result that gets mistaken for "production
// works".
func TestDryRunReportsImpersonation(t *testing.T) {
	p, db, prod := dryRunFixture(t)
	dgpo, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatalf("GetDGPO: %v", err)
	}

	// Our fixture set initminer's on-chain signing key to prod's key, so this
	// one is the genuine article.
	p.dryRunProduce(&prod, dgpo, dgpo.Time+3, prod.Account, true)
	if st := p.DryRunSnapshot(); st.ChainKeyOK != 1 || st.Imperson != 0 {
		t.Fatalf("owned key: chainKeyOK=%d impersonated=%d, want 1/0", st.ChainKeyOK, st.Imperson)
	}

	// Now sign for the same witness with a key the chain does not know.
	other := sha256.Sum256([]byte("not-the-witness-key"))
	wrong, err := crypto.PrivateKeyFromBytes(other[:])
	if err != nil {
		t.Fatalf("PrivateKeyFromBytes: %v", err)
	}
	imp := Producer{Account: prod.Account, PrivateKey: wrong, PublicKey33: wrong.PublicKey()}
	p.dryRunProduce(&imp, dgpo, dgpo.Time+3, prod.Account, true)

	st := p.DryRunSnapshot()
	if st.Imperson != 1 {
		t.Fatalf("impersonated=%d want 1 — a key the chain does not have for this witness must be reported", st.Imperson)
	}
	// Self-verification still passes (we signed with the key we declared); the
	// two checks answer different questions and must not be conflated.
	if st.VerifyOK != 2 {
		t.Fatalf("verifyOK=%d want 2 — self-verification is about OUR key, not the chain's", st.VerifyOK)
	}
}

// TestDryRunOneBlockPerSlot pins the dedupe. The production loop ticks every
// 500ms but a slot is 3s, so without per-slot dedupe a single slot would yield
// ~6 identical blocks and every timing average would be computed over
// duplicates.
//
// This matters because the dry run deliberately does NOT wait for the slot
// boundary (against a live chain that window is unwinnable — see
// maybeProduceBlock), so the tick rate alone no longer limits production.
func TestDryRunOneBlockPerSlot(t *testing.T) {
	p, _, _ := dryRunFixture(t)

	// Several ticks inside the same slot.
	for i := 0; i < 6; i++ {
		if err := p.maybeProduceBlock(); err != nil {
			t.Fatalf("maybeProduceBlock: %v", err)
		}
	}
	st := p.DryRunSnapshot()
	if st.Built != 1 {
		t.Fatalf("built=%d across 6 ticks in one slot, want exactly 1 (dedupe is not working)", st.Built)
	}

	// A new slot must produce again.
	p.mu.Lock()
	p.lastDrySlot = 0
	p.mu.Unlock()
	if err := p.maybeProduceBlock(); err != nil {
		t.Fatalf("maybeProduceBlock (next slot): %v", err)
	}
	if st = p.DryRunSnapshot(); st.Built != 2 {
		t.Fatalf("built=%d after a new slot, want 2", st.Built)
	}
}

// TestDryRunSkipsStaleHead pins the tip gate. Removing the slot-boundary wait
// (so the dry run can measure at all against a live chain) also removed the
// only thing keeping it off a stale head — a warm-started node then built
// blocks during catch-up, against a head minutes-to-hours old and the schedule
// as it stood back then. Those are not live production and they polluted the
// timing averages; observed as the first two "gtg slots" of a run.
func TestDryRunSkipsStaleHead(t *testing.T) {
	p, _, _ := dryRunFixture(t)
	// Use the real threshold: the genesis fixture's head is years old, so this
	// is exactly the catch-up case.
	p.DryRunMaxHeadAgeSec = 0

	for i := 0; i < 4; i++ {
		if err := p.maybeProduceBlock(); err != nil {
			t.Fatalf("maybeProduceBlock: %v", err)
		}
	}
	if st := p.DryRunSnapshot(); st.Built != 0 {
		t.Fatalf("built=%d against a years-old head, want 0 — the dry run must only measure at the tip", st.Built)
	}
}

// TestRealProductionRefusesWrongKey pins the gate that stands between "log and
// skip" and publishing a real consensus block.
//
// hived declines to produce when it lacks the private key for the witness's
// registered signing key, rather than emitting a block the network rejects.
// Producing with the wrong key is strictly worse than not producing: the block
// is rejected, the slot is missed anyway, and the slot is spent.
func TestRealProductionRefusesWrongKey(t *testing.T) {
	seed := sha256.Sum256([]byte("real-prod-gate"))
	chainKey, err := crypto.PrivateKeyFromBytes(seed[:])
	if err != nil {
		t.Fatalf("PrivateKeyFromBytes: %v", err)
	}
	// Fixture registers chainKey as initminer's on-chain signing key.
	cdb, db := newSigningFixture(t, chainKey)

	// We hold a DIFFERENT key — the situation when a witness is configured with
	// a stale or wrong signing key.
	other := sha256.Sum256([]byte("a-key-the-chain-does-not-know"))
	wrong, err := crypto.PrivateKeyFromBytes(other[:])
	if err != nil {
		t.Fatalf("PrivateKeyFromBytes: %v", err)
	}
	prod := Producer{Account: genesis.MainnetInitminer, PrivateKey: wrong, PublicKey33: wrong.PublicKey()}

	rec := &recordingPublisher{}
	p := New(cdb, db, []Producer{prod})
	p.Publisher = rec // REAL production mode: DryRun stays false

	before, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatalf("GetDGPO: %v", err)
	}
	for i := 0; i < 4; i++ {
		if err := p.maybeProduceBlock(); err != nil {
			t.Fatalf("maybeProduceBlock: %v", err)
		}
	}

	if n := p.SkippedNoKey(); n == 0 {
		t.Fatal("wrong key: no slots recorded as skipped — the key gate did not fire")
	}
	if rec.calls != 0 {
		t.Fatalf("wrong key: published %d block(s) — a block the network would reject must never be published", rec.calls)
	}
	after, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatalf("GetDGPO after: %v", err)
	}
	if after.HeadBlockNumber != before.HeadBlockNumber {
		t.Fatalf("wrong key: head advanced %d -> %d; nothing should have been produced",
			before.HeadBlockNumber, after.HeadBlockNumber)
	}
}

// TestProductionDoesNotApplyLocally pins the fix for the 2026-08-10 mainnet
// wedge (howo, block 108887429).
//
// The witness used to call chain.ApplyBlock on its own block, with no
// Reversible flag. That made a block we produced the COMMITTED head the instant
// we built it — and nothing can reorg away a committed block. When the network
// chose a different block for that height, chain.PushLiveBlock saw
// num <= committedNum() and filed the real block PushStale; its child then
// buffered forever waiting for a parent we would never apply. The node sat on
// its own orphan indefinitely with --fork-switching ON and unable to help.
//
// The invariant that prevents a rerun: producing must mutate NO chain state
// here. Publication is the single hand-off, and the apply happens on the sync
// delegate's single-writer goroutine via PushLiveBlock, which keeps our tip
// reversible and therefore reorg-able.
func TestProductionDoesNotApplyLocally(t *testing.T) {
	seed := sha256.Sum256([]byte("real-prod-applies-nothing"))
	chainKey, err := crypto.PrivateKeyFromBytes(seed[:])
	if err != nil {
		t.Fatalf("PrivateKeyFromBytes: %v", err)
	}
	// Fixture registers chainKey on chain AND we hold it, so the key gate opens
	// and the producing path runs for real.
	cdb, db := newSigningFixture(t, chainKey)
	prod := Producer{
		Account:     genesis.MainnetInitminer,
		PrivateKey:  chainKey,
		PublicKey33: chainKey.PublicKey(),
	}
	rec := &recordingPublisher{}
	p := New(cdb, db, []Producer{prod})
	p.Publisher = rec // DryRun stays false: this is the REAL production path

	before, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatalf("GetDGPO: %v", err)
	}
	if err := p.maybeProduceBlock(); err != nil {
		t.Fatalf("maybeProduceBlock: %v", err)
	}

	if rec.calls != 1 {
		t.Fatalf("published %d block(s), want 1 — the key matched, so the block should have been handed to the publisher", rec.calls)
	}
	if n := p.Produced(); n != 1 {
		t.Fatalf("produced counter = %d, want 1", n)
	}

	after, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatalf("GetDGPO after: %v", err)
	}
	if after.HeadBlockNumber != before.HeadBlockNumber {
		t.Fatalf("witness applied its own block locally: head %d -> %d. "+
			"That commits the block irreversibly and is exactly what wedged the node at 108887429; "+
			"the block must be applied by the sync delegate via PushLiveBlock instead",
			before.HeadBlockNumber, after.HeadBlockNumber)
	}
	if after.HeadBlockID != before.HeadBlockID {
		t.Fatal("witness changed head_block_id; production must not mutate chain state directly")
	}
}

// TestProductionRefusesWithoutPublisher pins the other silent-failure mode. A
// witness with no publisher wired must not build blocks into the void: on
// 2026-08-10 a --produce run spent 47 minutes doing nothing at all because the
// plugin was never constructed, and every health metric stayed green. Absence
// of work must be reported, not inferred.
func TestProductionRefusesWithoutPublisher(t *testing.T) {
	seed := sha256.Sum256([]byte("real-prod-no-publisher"))
	chainKey, err := crypto.PrivateKeyFromBytes(seed[:])
	if err != nil {
		t.Fatalf("PrivateKeyFromBytes: %v", err)
	}
	cdb, db := newSigningFixture(t, chainKey)
	prod := Producer{
		Account:     genesis.MainnetInitminer,
		PrivateKey:  chainKey,
		PublicKey33: chainKey.PublicKey(),
	}
	p := New(cdb, db, []Producer{prod}) // Publisher deliberately nil

	before, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatalf("GetDGPO: %v", err)
	}
	if err := p.maybeProduceBlock(); err != nil {
		t.Fatalf("maybeProduceBlock: %v", err)
	}
	if n := p.Produced(); n != 0 {
		t.Fatalf("produced=%d with no publisher wired; a block that cannot be published must not be built", n)
	}
	after, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatalf("GetDGPO after: %v", err)
	}
	if after.HeadBlockNumber != before.HeadBlockNumber {
		t.Fatalf("head advanced %d -> %d with no publisher", before.HeadBlockNumber, after.HeadBlockNumber)
	}
}

// TestNextWakeAlignsToSlotBoundary pins the production loop's timing.
//
// The loop used to poll on a fixed 500ms ticker, so work could begin up to
// 500ms after the slot instant — ~17% of the 3s budget gone before assembling
// anything, while hived arms a timer for the exact boundary. nextWake sleeps to
// the boundary when it is near, and otherwise caps the sleep so we keep
// retrying: a boundary check that finds the previous block hasn't landed yet
// must not then wait a whole further slot.
func TestNextWakeAlignsToSlotBoundary(t *testing.T) {
	slot := int64(BlockInterval)

	// Exactly ON a boundary: the next one is a full slot away, so the cap applies.
	onBoundary := time.Unix(0, (time.Now().UnixNano()/slot)*slot)
	if got := nextWake(onBoundary); got != maxProducePoll {
		t.Fatalf("nextWake(on boundary) = %v, want the %v poll cap", got, maxProducePoll)
	}

	// Just before a boundary: sleep exactly the remainder, so we wake ON it.
	for _, remain := range []time.Duration{1 * time.Millisecond, 100 * time.Millisecond} {
		base := time.Unix(0, (time.Now().UnixNano()/slot+1)*slot).Add(-remain)
		// Land just PAST the instant: waking exactly on it loses the
		// now >= slotTime race to timer jitter and misses the slot entirely.
		if got := nextWake(base); got != remain+slotWakeSkew {
			t.Fatalf("nextWake(%v before boundary) = %v, want %v (boundary + skew)",
				remain, got, remain+slotWakeSkew)
		}
	}

	// Never sleeps past the cap, and never returns a non-positive delay (which
	// would spin the loop).
	for off := time.Duration(0); off < BlockInterval; off += 37 * time.Millisecond {
		got := nextWake(time.Unix(0, (time.Now().UnixNano()/slot)*slot).Add(off))
		if got <= 0 || got > maxProducePoll+slotWakeSkew {
			t.Fatalf("nextWake(offset %v) = %v, want 0 < d <= %v", off, got, maxProducePoll+slotWakeSkew)
		}
	}
}
