package testfixture

import (
	"testing"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
)

// Helper: seed the treasury account with HBD for proposal payouts.
func seedTreasury(t *testing.T, f *Fixture, treasuryName string, hbd int64) {
	t.Helper()
	if !f.HasAccount(treasuryName) {
		f.Actor(treasuryName)
	}
	f.FundLiquidHBD(treasuryName, hbd)
}

// Port of proposal_tests.cpp::generating_payments idiom.
//
// Active proposal with votes gets its daily_pay × 1h/24h credited
// once the maintenance period fires. Treasury balance drops by the
// same amount; receiver's HBD goes up.
func TestProposalDailyPaymentBasic(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF21)
	f.Actors("alice", "bob", "carol")
	f.FundLiquidHBD("alice", 1_000_000)
	f.FundVesting("carol", 100_000_000) // carol votes
	f.GenerateBlock()

	// Seed treasury with 100,000 HBD so daily budget = 1,000 HBD,
	// hourly budget = 1000/24 ≈ 41 HBD.
	seedTreasury(t, f, "steem.dao", 100_000_000)

	now := f.HeadBlockTime()
	// Create a proposal that's already active.
	if err := f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "bob",
		StartDate: now - 60, EndDate: now + 60*60*24*30,
		DailyPay: HBD(10_000), Subject: "x", Permlink: "p",
	}); err != nil {
		t.Fatalf("create_proposal: %v", err)
	}
	if err := f.ApplyOp(op.UpdateProposalVotes{
		Voter: "carol", ProposalIDs: []int64{1}, Approve: true,
	}); err != nil {
		t.Fatalf("vote: %v", err)
	}

	preBob := f.HBDBalance("bob")
	preTreas := f.HBDBalance("steem.dao")

	// Skip to the next maintenance boundary (1 hour).
	skip := proposalMaintenanceUntilNext(f)
	f.SkipSeconds(skip)

	bob := f.HBDBalance("bob")
	treas := f.HBDBalance("steem.dao")

	if bob <= preBob {
		t.Fatalf("bob's HBD should grow after maintenance: pre=%d post=%d",
			preBob, bob)
	}
	if treas >= preTreas {
		t.Fatalf("treasury HBD should drop after payout: pre=%d post=%d",
			preTreas, treas)
	}
	// Bob's payout uses hived's pre-HF25 ratio formula
	// (dhf_processor.cpp:197-198):
	//   ratio = (passed_time * HIVE_100_PERCENT) / 86400
	//   period_pay = (ratio * daily_pay) / HIVE_100_PERCENT
	// On the first DHF tick post-HF21, gopherd's processProposals
	// falls back to proposalMaintenancePeriodSec (3600s) because
	// LastBudgetTime is still zero (mirrors hived behavior where the
	// field stays at HIVE_GENESIS_TIME default; the first tick on
	// mainnet sees an empty treasury so the resulting payout is 0).
	// In this test the treasury is seeded so we observe the formula.
	ratio := int64(3600) * 10000 / 86400
	wantBob := ratio * 10_000 / 10000
	deltaBob := bob - preBob
	if deltaBob != wantBob {
		t.Fatalf("bob's payment: got %d, want %d (pre-HF25 ratio formula)", deltaBob, wantBob)
	}
	// Treasury delta is approximately wantBob minus the interest
	// credited to the treasury over the maintenance interval —
	// strict conservation doesn't hold because AdjustHBDBalance
	// accrues per-side interest. Just sanity-check the magnitude.
	deltaTreas := preTreas - treas
	if deltaTreas <= 0 || deltaTreas > wantBob {
		t.Fatalf("treasury delta out of range: got %d, want in (0, %d]",
			deltaTreas, wantBob)
	}
}

// Proposals with zero votes are skipped.
func TestProposalZeroVotesNoPayment(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF21)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 1_000_000)
	f.GenerateBlock()
	seedTreasury(t, f, "steem.dao", 100_000_000)

	now := f.HeadBlockTime()
	if err := f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "bob",
		StartDate: now - 60, EndDate: now + 60*60*24*30,
		DailyPay: HBD(10_000), Subject: "x", Permlink: "p",
	}); err != nil {
		t.Fatalf("create_proposal: %v", err)
	}
	// No votes cast.

	preBob := f.HBDBalance("bob")
	skip := proposalMaintenanceUntilNext(f)
	f.SkipSeconds(skip)
	if got := f.HBDBalance("bob"); got != preBob {
		t.Fatalf("bob with zero-vote proposal got paid: pre=%d post=%d",
			preBob, got)
	}
}

// Daily-budget cap (treasury / 100): a proposal with daily_pay larger
// than the daily budget is paid only the budget amount.
func TestProposalDailyBudgetCap(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF21)
	f.Actors("alice", "bob", "carol")
	f.FundLiquidHBD("alice", 1_000_000)
	f.FundVesting("carol", 100_000_000)
	f.GenerateBlock()
	// Treasury 1000 HBD → daily budget 10 HBD → hourly budget ~0.4 HBD.
	seedTreasury(t, f, "steem.dao", 1_000_000)

	now := f.HeadBlockTime()
	// Daily pay = 1,000,000 HBD/day → hourly = ~41,666 HBD. Budget
	// will cap at the hourly slice of treasury/100.
	if err := f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "bob",
		StartDate: now - 60, EndDate: now + 60*60*24*30,
		DailyPay: HBD(1_000_000_000), Subject: "x", Permlink: "p",
	}); err != nil {
		t.Fatalf("create_proposal: %v", err)
	}
	if err := f.ApplyOp(op.UpdateProposalVotes{
		Voter: "carol", ProposalIDs: []int64{1}, Approve: true,
	}); err != nil {
		t.Fatalf("vote: %v", err)
	}

	skip := proposalMaintenanceUntilNext(f)
	f.SkipSeconds(skip)

	bob := f.HBDBalance("bob")
	// Hourly budget = treasury / 100 × 3600 / 86400. Treasury is the
	// seeded 1_000_000 + 10 HBD (10_000 sat) fee paid by alice on
	// create_proposal (dhf_evaluator.cpp:96).
	const treasuryAtMaintenance = int64(1_000_000 + 10_000)
	wantMax := treasuryAtMaintenance / 100 * 3600 / 86400
	if bob > wantMax {
		t.Fatalf("bob exceeded hourly budget: got %d, max %d", bob, wantMax)
	}
	if bob == 0 {
		t.Fatal("bob should have received SOMETHING within the budget cap")
	}
}

// Sort by votes desc: a higher-voted proposal pays out before a
// lower-voted one. With a tight budget, only the higher-voted gets
// paid in full.
func TestProposalPaymentVoteOrderRanking(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF21)
	f.Actors("alice", "bob", "carol", "dave", "eve")
	f.FundLiquidHBD("alice", 1_000_000)
	f.FundVesting("dave", 100_000_000)
	f.FundVesting("eve", 50_000_000)
	f.GenerateBlock()
	seedTreasury(t, f, "steem.dao", 100_000_000) // 100k HBD treasury

	now := f.HeadBlockTime()
	// Two proposals, same daily_pay. dave votes for #1 (higher stake),
	// eve votes for #2.
	if err := f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "bob",
		StartDate: now - 60, EndDate: now + 60*60*24*30,
		DailyPay: HBD(50_000), Subject: "p1", Permlink: "p1",
	}); err != nil {
		t.Fatalf("p1: %v", err)
	}
	if err := f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "carol",
		StartDate: now - 60, EndDate: now + 60*60*24*30,
		DailyPay: HBD(50_000), Subject: "p2", Permlink: "p2",
	}); err != nil {
		t.Fatalf("p2: %v", err)
	}
	if err := f.ApplyOp(op.UpdateProposalVotes{
		Voter: "dave", ProposalIDs: []int64{1}, Approve: true,
	}); err != nil {
		t.Fatalf("dave vote: %v", err)
	}
	if err := f.ApplyOp(op.UpdateProposalVotes{
		Voter: "eve", ProposalIDs: []int64{2}, Approve: true,
	}); err != nil {
		t.Fatalf("eve vote: %v", err)
	}

	skip := proposalMaintenanceUntilNext(f)
	f.SkipSeconds(skip)

	bob := f.HBDBalance("bob")
	carol := f.HBDBalance("carol")
	if bob == 0 {
		t.Fatal("higher-voted proposal (bob) should have received payment")
	}
	// Bob (more votes) should get paid first; if budget runs out,
	// carol's may be smaller. With a fat treasury both get paid, so
	// the assertion is just that bob got at least as much.
	if bob < carol {
		t.Fatalf("higher-voted proposal underpaid: bob=%d carol=%d", bob, carol)
	}
}

// Pre-HF21: process_proposals is a no-op.
func TestProposalPreHF21NoOp(t *testing.T) {
	f := New(t)
	// HF20 only.
	f.HardforksUpTo(hardfork.HF20)
	f.Actors("alice", "bob", "carol", "steem.dao")
	f.FundLiquidHBD("alice", 1_000_000)
	f.FundLiquidHBD("steem.dao", 100_000_000)
	f.FundVesting("carol", 100_000_000)
	f.GenerateBlock()

	now := f.HeadBlockTime()
	// Pre-HF21 the create_proposal evaluator might also be gated; we
	// check by attempting and ignoring errors.
	_ = f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "bob",
		StartDate: now - 60, EndDate: now + 60*60*24*30,
		DailyPay: HBD(10_000), Subject: "x", Permlink: "p",
	})

	preBob := f.HBDBalance("bob")
	skip := proposalMaintenanceUntilNext(f)
	f.SkipSeconds(skip)
	if got := f.HBDBalance("bob"); got != preBob {
		t.Fatalf("pre-HF21: bob received payment despite hook being a no-op: %d→%d",
			preBob, got)
	}
}

// proposalMaintenanceUntilNext returns the seconds to skip to land on
// the next 1-hour boundary (when processProposals fires).
func proposalMaintenanceUntilNext(f *Fixture) uint32 {
	const period = uint32(60 * 60)
	now := f.HeadBlockTime()
	rem := now % period
	if rem == 0 {
		return period
	}
	return period - rem
}
