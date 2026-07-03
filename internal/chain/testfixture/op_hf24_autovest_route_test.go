package testfixture

import (
	"testing"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
)

// Post-HF24, an auto_vest withdraw-route deposit adds NEW vesting to the
// recipient, so — like transfer_to_vesting and every other HF24 vesting
// credit — its witness-vote weight must be DEFERRED via delayed_voting
// (hived database_vesting.cpp:208-217 → dv->add_votes → add_delayed_value),
// not applied immediately and not dropped.
//
// Regression guard for the wall-bug fix (712d1a7): the old code only did
// the immediate adjust_proxied_witness_votes pre-HF24 and skipped the
// deposit's vote propagation ENTIRELY post-HF24, permanently under-counting
// every auto_vest-route recipient's voted witnesses. That flipped a
// timeshare witness pick at block 48,905,325 and cascaded to the
// claim_reward_balance failures at 49,596,514.
func TestHF24AutoVestRouteDelayedVoting(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF24)
	f.Actors("alice", "bob", "sam")
	f.FundLiquid("sam", 5_000)
	f.GenerateBlock()

	// sam is a witness; bob votes for sam. bob has no vesting yet, so
	// sam gets 0 governance weight from bob for now.
	if err := f.ApplyOp(op.WitnessUpdate{
		Owner:           "sam",
		URL:             "foo.bar",
		BlockSigningKey: [33]byte{0x02, 0x01},
		Props: op.LegacyChainProperties{
			AccountCreationFee: LegacyHIVE(1_000),
			MaximumBlockSize:   65536,
			HBDInterestRate:    1000,
		},
		Fee: HIVE(1_000),
	}); err != nil {
		t.Fatalf("witness_create sam: %v", err)
	}
	if err := f.ApplyOp(op.AccountWitnessVote{Account: "bob", Witness: "sam", Approve: true}); err != nil {
		t.Fatalf("bob→sam witness vote: %v", err)
	}
	if got := f.Witness("sam").TotalVotesVests; got != 0 {
		t.Fatalf("sam.TotalVotesVests pre-deposit: got %d, want 0 (bob has no vests)", got)
	}

	// alice powers down all her vesting, routing 100% to bob as VESTS.
	f.FundVesting("alice", 1_040_000)
	f.GenerateBlock()
	startVests := f.Vests("alice")
	if startVests == 0 {
		t.Fatal("alice should have VESTS after FundVesting")
	}
	if err := f.ApplyOp(op.WithdrawVesting{Account: "alice", VestingShares: VESTS(startVests)}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	rate := f.Account_("alice").VestingWithdrawRate
	if rate <= 0 {
		t.Fatalf("VestingWithdrawRate should be set, got %d", rate)
	}
	if err := f.ApplyOp(op.SetWithdrawVestingRoute{
		FromAccount: "alice", ToAccount: "bob", Percent: 10000, AutoVest: true,
	}); err != nil {
		t.Fatalf("route bob 100%%: %v", err)
	}

	preBobVests := f.Vests("bob")
	if pre := f.Account_("bob").SumDelayedVotes; pre != 0 {
		t.Fatalf("bob.SumDelayedVotes pre-chunk: got %d, want 0", pre)
	}

	// Fire the first weekly withdrawal chunk.
	f.SkipSeconds(60*60*24*7 + 60)

	bob := f.Account_("bob")
	bobDelta := bob.VestingShares - preBobVests
	if bobDelta <= 0 {
		t.Fatalf("bob VESTS should rise from the auto_vest route, delta=%d", bobDelta)
	}

	// THE FIX: the deposit must be queued in bob's delayed_votes, not
	// dropped. Pre-fix bob.SumDelayedVotes stayed 0 here.
	if bob.SumDelayedVotes == 0 {
		t.Fatal("bob.SumDelayedVotes should be non-zero post-HF24 (auto_vest deposit must be delayed-voted, not dropped)")
	}
	if bob.SumDelayedVotes != uint64(bob.VestingShares) {
		t.Fatalf("bob.SumDelayedVotes %d != bob.VestingShares %d (all bob's vesting came from the just-deposited chunk)",
			bob.SumDelayedVotes, bob.VestingShares)
	}
	if len(bob.DelayedVotes) == 0 {
		t.Fatal("bob.DelayedVotes should have at least one bucket")
	}

	// Still delayed → sam's witness total must NOT have moved yet.
	if got := f.Witness("sam").TotalVotesVests; got != 0 {
		t.Fatalf("sam.TotalVotesVests right after deposit: got %d, want 0 (deposit is delayed, not immediate)", got)
	}

	// After the 30-day maturation window the earliest bucket matures and
	// propagates to sam (the witness bob votes for).
	f.SkipSeconds(60*60*24*30 + 120)
	if got := f.Witness("sam").TotalVotesVests; got == 0 {
		t.Fatal("sam.TotalVotesVests should be > 0 after the 30-day delayed-vote maturation (matured auto_vest deposit reaches bob's voted witness)")
	}
}
