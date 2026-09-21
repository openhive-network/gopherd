package main

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
)

// The simulation is what the verdict rests on, so its arithmetic is pinned
// here rather than trusted. A scan that under-counts reports "no divergence"
// for a chain that has one, which is the expensive direction to be wrong in.

// TestGovAnchorMatchesTheHardforkTable is the test that should have existed
// first. The original only checked govExpirationFor against govFirstExpireTimestamp
// — the tool's formula against the tool's own constant — so it passed happily
// while that constant was wrong by 7200 seconds, translating every pre-HF25
// account's expiry by 2400 slots. Self-consistency is not correctness.
func TestGovAnchorMatchesTheHardforkTable(t *testing.T) {
	want := hardfork.MainnetActivationTime[hardfork.HF25] + govVoteExpirationPeriodSec
	if govFirstExpireTimestamp != want {
		t.Fatalf("anchor: got %d, want %d (HF25 time + one year)", govFirstExpireTimestamp, want)
	}
	// And the table itself must still say what the evaluator assumes.
	const hf25Time = 1625061600 // HIVE_HARDFORK_1_25_TIME, hardfork.d/1_25.hf:10
	if got := hardfork.MainnetActivationTime[hardfork.HF25]; got != hf25Time {
		t.Errorf("HF25 activation time: got %d, want %d", got, hf25Time)
	}
}

func TestGovExpirationMatchesEvaluator(t *testing.T) {
	// Post-HF25 action: plain one-year offset, no redistribution.
	post := uint32(1700000000)
	if got, want := govExpirationFor(post), post+govVoteExpirationPeriodSec; got != want {
		t.Errorf("post-HF25: got %d, want %d", got, want)
	}

	// Pre-HF25 action: the one-year offset lands before the first expiry
	// timestamp, so it is redistributed across the six-month window instead of
	// clustering everyone on the HF25 anniversary.
	pre := uint32(1600000000)
	raw := pre + govVoteExpirationPeriodSec
	if raw >= govFirstExpireTimestamp {
		t.Fatal("setup: this timestamp no longer exercises the redistribution branch")
	}
	want := govFirstExpireTimestamp + (raw % govMaxOldExpireShiftSec)
	if got := govExpirationFor(pre); got != want {
		t.Errorf("pre-HF25: got %d, want %d", got, want)
	}
	if got := govExpirationFor(pre); got < govFirstExpireTimestamp {
		t.Errorf("redistributed stamp %d fell before the first expiry timestamp", got)
	}
}

func TestGovSimulateCountsVotesNotAccounts(t *testing.T) {
	ts := uint32(1700000000)
	// One account with three proposal votes: the budget counts VOTES, so this
	// is three objects, not one.
	events := []govEvent{
		{block: 1, ts: ts, kind: govEvtProposalVote, account: "alice", proposal: 1, approve: true},
		{block: 1, ts: ts, kind: govEvtProposalVote, account: "alice", proposal: 2, approve: true},
		{block: 1, ts: ts, kind: govEvtProposalVote, account: "alice", proposal: 3, approve: true},
	}
	res := govSimulate(events, 200)
	if res.maxPerSlot != 3 {
		t.Errorf("votes in the busiest slot: got %d, want 3", res.maxPerSlot)
	}
	if res.maxAccounts != 1 {
		t.Errorf("accounts in the busiest slot: got %d, want 1", res.maxAccounts)
	}
}

func TestGovSimulateUnapproveRemovesTheVote(t *testing.T) {
	ts := uint32(1700000000)
	events := []govEvent{
		{block: 1, ts: ts, kind: govEvtProposalVote, account: "alice", proposal: 7, approve: true},
		{block: 2, ts: ts, kind: govEvtProposalVote, account: "alice", proposal: 7, approve: false},
	}
	if res := govSimulate(events, 200); res.maxPerSlot != 0 || res.votersSeen != 0 {
		t.Errorf("an unapproved vote still counted: %+v", res)
	}
}

// A removed proposal erases every vote cast on it, so those votes are no
// longer the expiry sweep's to remove. Missing this would inflate the count
// and manufacture a divergence that is not there.
func TestGovSimulateProposalRemovalCascades(t *testing.T) {
	ts := uint32(1700000000)
	events := []govEvent{
		{block: 1, ts: ts, kind: govEvtProposalVote, account: "alice", proposal: 5, approve: true},
		{block: 1, ts: ts, kind: govEvtProposalVote, account: "bob", proposal: 5, approve: true},
		{block: 1, ts: ts, kind: govEvtProposalVote, account: "bob", proposal: 6, approve: true},
		{block: 2, ts: ts, kind: govEvtProposalRemoved, proposal: 5},
	}
	res := govSimulate(events, 200)
	if res.maxPerSlot != 1 {
		t.Errorf("after removing proposal 5 only bob's vote on 6 survives: got %d, want 1", res.maxPerSlot)
	}
	if res.votersSeen != 1 {
		t.Errorf("voters with surviving votes: got %d, want 1", res.votersSeen)
	}
}

// A later governance action of ANY kind restamps the expiration, which moves
// the account into a different sweep. Treating the proposal vote as the only
// restamping action would bucket accounts into the wrong block.
func TestGovSimulateLaterActionMovesTheSlot(t *testing.T) {
	early := uint32(1700000000)
	late := early + 5*govBlockIntervalSec
	events := []govEvent{
		{block: 1, ts: early, kind: govEvtProposalVote, account: "alice", proposal: 1, approve: true},
		{block: 2, ts: late, kind: govEvtGovernanceAction, account: "alice"},
	}
	res := govSimulate(events, 200)
	wantSlot := govExpirationFor(late) / govBlockIntervalSec * govBlockIntervalSec
	if res.maxSlotTime != wantSlot {
		t.Errorf("slot: got %d, want %d — the witness vote did not restamp the expiry",
			res.maxSlotTime, wantSlot)
	}
}

// Accounts whose expirations land in the same three-second slot are swept by
// the same block, so their votes share one budget. Splitting them per account
// is how a real divergence gets reported as clean.
func TestGovSimulateSameSlotSharesTheBudget(t *testing.T) {
	ts := uint32(1700000000)
	var events []govEvent
	for i := 0; i < 150; i++ {
		name := "voter" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		events = append(events,
			govEvent{block: 1, ts: ts, kind: govEvtProposalVote, account: name, proposal: 1, approve: true},
			govEvent{block: 1, ts: ts, kind: govEvtProposalVote, account: name, proposal: 2, approve: true})
	}
	res := govSimulate(events, 200)
	if res.maxPerSlot != 300 {
		t.Fatalf("150 accounts x 2 votes in one slot: got %d, want 300", res.maxPerSlot)
	}
	if res.overBudget != 1 {
		t.Errorf("300 > 200 must be flagged as over budget, got %d", res.overBudget)
	}
	if res.maxAccounts != 150 {
		t.Errorf("accounts in the slot: got %d, want 150", res.maxAccounts)
	}
}

func TestGovSimulateAtBudgetIsNotOver(t *testing.T) {
	ts := uint32(1700000000)
	var events []govEvent
	for i := 0; i < 200; i++ {
		events = append(events, govEvent{
			block: 1, ts: ts, kind: govEvtProposalVote,
			account: "alice", proposal: int64(i), approve: true,
		})
	}
	res := govSimulate(events, 200)
	if res.maxPerSlot != 200 {
		t.Fatalf("got %d votes, want 200", res.maxPerSlot)
	}
	if res.overBudget != 0 {
		t.Errorf("exactly at the budget must not be flagged: got %d", res.overBudget)
	}
}

// TestGovSimulateMergesConsecutiveSlots covers the missed-block case. Slots
// are one block each only while the chain never misses one; every missed block
// makes the next block cover the skipped slot too, so its sweep faces both
// slots' accounts at once. A verdict built on single slots alone would
// under-count exactly when the chain is unhealthy — which is when a clump is
// most likely.
func TestGovSimulateMergesConsecutiveSlots(t *testing.T) {
	base := uint32(1700000000)
	// Two accounts whose expirations land in ADJACENT slots, 150 votes each.
	// Neither slot alone reaches the budget; one missed block between them
	// puts all 300 in front of a single sweep.
	var events []govEvent
	add := func(account string, actionTs uint32, votes int) {
		for i := 0; i < votes; i++ {
			events = append(events, govEvent{
				block: 1, ts: actionTs, kind: govEvtProposalVote,
				account: account, proposal: int64(i), approve: true,
			})
		}
	}
	add("alice", base, 150)
	// One slot later: the expiration is a pure offset of the action time, so
	// shifting the action by one slot shifts the expiry by one slot.
	add("bob", base+govBlockIntervalSec, 150)

	res := govSimulate(events, 200)
	if res.maxPerSlot != 150 {
		t.Fatalf("single-slot max: got %d, want 150", res.maxPerSlot)
	}
	if res.overBudget != 0 {
		t.Errorf("neither slot alone exceeds the budget, got %d over", res.overBudget)
	}
	if res.maxPerWindow != 300 {
		t.Errorf("merged window: got %d, want 300 — adjacent slots must be summed", res.maxPerWindow)
	}
	if res.overBudgetWindow == 0 {
		t.Error("300 across two adjacent slots exceeds 200 and must be flagged")
	}
}

// TestGovSimulateWindowRespectsRealGaps — the window slides over slot NUMBERS,
// not over list positions. Two clumps a year apart are never swept together,
// and summing them would invent a divergence.
func TestGovSimulateWindowRespectsRealGaps(t *testing.T) {
	base := uint32(1700000000)
	var events []govEvent
	add := func(account string, actionTs uint32, votes int) {
		for i := 0; i < votes; i++ {
			events = append(events, govEvent{
				block: 1, ts: actionTs, kind: govEvtProposalVote,
				account: account, proposal: int64(i), approve: true,
			})
		}
	}
	add("alice", base, 150)
	add("bob", base+govMergeWindowSlots*govBlockIntervalSec*100, 150)

	res := govSimulate(events, 200)
	if res.maxPerWindow != 150 {
		t.Errorf("far-apart clumps were summed: window max %d, want 150", res.maxPerWindow)
	}
	if res.overBudgetWindow != 0 {
		t.Errorf("no window should be over budget, got %d", res.overBudgetWindow)
	}
}
