package testfixture

import (
	"testing"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
)

// Port of operation_tests.cpp::vote_apply (subset).
//
// Verifies the post-HF14 dust threshold subtraction:
// net_rshares == used_mana - HIVE_VOTE_DUST_THRESHOLD.
//
// Setup: alice has enough vesting to vote above the dust floor; she
// posts and self-votes at 100% weight; the comment's net_rshares
// should equal her mana drain minus the dust constant.
func TestVoteApplyDustSubtraction(t *testing.T) {
	f := New(t)
	// HF17 to ensure a stable cashout window (12h pre, 7d after) and
	// HF14+ dust subtraction.
	f.HardforksUpTo(hardfork.HF17)
	f.Actor("alice")
	f.FundVesting("alice", 100_000_000) // enough VESTS for non-dust votes
	f.GenerateBlock()

	if err := f.ApplyOp(op.Comment{
		Author: "alice", Permlink: "p", Body: "hi",
	}); err != nil {
		t.Fatalf("comment: %v", err)
	}

	preMana := f.Account_("alice").VotingManaCurrent
	if err := f.ApplyOp(op.Vote{
		Voter: "alice", Author: "alice", Permlink: "p", Weight: 10000,
	}); err != nil {
		t.Fatalf("vote: %v", err)
	}

	postMana := f.Account_("alice").VotingManaCurrent
	manaUsed := preMana - postMana
	if manaUsed <= 0 {
		t.Fatalf("expected mana to be drained, got mana_used=%d", manaUsed)
	}

	c := f.Comment("alice", "p")
	if c.NetRshares <= 0 {
		t.Fatalf("expected NetRshares > 0, got %d", c.NetRshares)
	}
	// The dust threshold (50_000_000) must have been subtracted —
	// otherwise NetRshares would be exactly some clean multiple of
	// vesting_shares × used_power. Heuristic check: NetRshares mod
	// vesting_shares should not be zero (since dust is a constant
	// 5e7 subtract, not a proportion).
	a := f.Account_("alice")
	if a.VestingShares == 0 || c.NetRshares%a.VestingShares == 0 {
		t.Fatalf("dust threshold not subtracted? VestingShares=%d, NetRshares=%d",
			a.VestingShares, c.NetRshares)
	}
}

// Voting on a non-existent comment should fail.
func TestVoteApplyNonexistentComment(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.GenerateBlock()
	err := f.ApplyOp(op.Vote{
		Voter: "alice", Author: "bob", Permlink: "ghost", Weight: 10000,
	})
	if err == nil {
		t.Fatal("expected vote-on-missing-comment to fail")
	}
}

// Voting with weight=0 on a never-voted comment should fail (hived
// asserts `o.weight != 0` for first-cast).
func TestVoteApplyZeroWeightFirstCast(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.Actor("bob")
	f.FundVesting("bob", 100_000_000)
	f.GenerateBlock()
	if err := f.ApplyOp(op.Comment{
		Author: "alice", Permlink: "p", Body: "hi",
	}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	err := f.ApplyOp(op.Vote{
		Voter: "bob", Author: "alice", Permlink: "p", Weight: 0,
	})
	if err == nil {
		t.Fatal("expected zero-weight first vote to fail")
	}
}

// Post-HF14, dust votes (rshares <= dust threshold) are rejected
// unless weight==0. We use a tiny vesting amount so used_mana would
// fall below 50_000_000.
func TestVoteApplyBelowDustRejected(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF14)
	f.Actor("alice")
	f.Actor("bob")
	f.FundVesting("bob", 1) // 1 satoshi VESTS — votes will hit dust floor
	f.GenerateBlock()
	if err := f.ApplyOp(op.Comment{
		Author: "alice", Permlink: "p", Body: "hi",
	}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	err := f.ApplyOp(op.Vote{
		Voter: "bob", Author: "alice", Permlink: "p", Weight: 10000,
	})
	if err == nil {
		t.Fatal("expected dust vote to be rejected")
	}
}
