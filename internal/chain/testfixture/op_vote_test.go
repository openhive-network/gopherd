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

// Two consecutive votes by the same voter on different comments must
// each drain mana — the second vote's currentPower (and therefore its
// abs_rshares) must be strictly LESS than the first's.
//
// Investigates the suspected mana-drain persistence bug found at
// craig-grant 3,540,652 (see feedback_craig_grant_cashout_drift.md):
// `juneaugoldbuyer` voted on two comments 96 sec apart and Go logged
// the same abs_rshares for both — suggesting voter.VotingManaCurrent
// wasn't being persisted between txs.
func TestVoteApplyConsecutiveDrainsMana(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF12) // pre-HF14 so no dust subtraction
	f.Actor("alice")
	f.Actor("bob")
	f.FundVesting("alice", 100_000_000)
	f.GenerateBlock()

	// Two posts by bob to vote on. Need 5min gap between root posts post-HF6.
	if err := f.ApplyOp(op.Comment{Author: "bob", Permlink: "p1", Body: "hi"}); err != nil {
		t.Fatalf("comment p1: %v", err)
	}
	f.GenerateBlock()
	f.SkipSeconds(6 * 60) // wait 6 minutes
	if err := f.ApplyOp(op.Comment{Author: "bob", Permlink: "p2", Body: "hi"}); err != nil {
		t.Fatalf("comment p2: %v", err)
	}
	f.GenerateBlock()

	manaBefore := f.Account_("alice").VotingManaCurrent
	if err := f.ApplyOp(op.Vote{Voter: "alice", Author: "bob", Permlink: "p1", Weight: 10000}); err != nil {
		t.Fatalf("vote 1: %v", err)
	}
	manaAfterVote1 := f.Account_("alice").VotingManaCurrent
	if manaAfterVote1 >= manaBefore {
		t.Fatalf("vote 1 didn't drain mana: before=%d, after=%d", manaBefore, manaAfterVote1)
	}

	// Skip ~3 seconds (one block) to mimic the production scenario.
	f.GenerateBlock()

	manaBeforeVote2 := f.Account_("alice").VotingManaCurrent
	if manaBeforeVote2 > manaAfterVote1+5 {
		t.Fatalf("voter mana regenerated too aggressively or wasn't persisted: vote1_after=%d, vote2_before=%d (delta=%d)",
			manaAfterVote1, manaBeforeVote2, manaBeforeVote2-manaAfterVote1)
	}

	if err := f.ApplyOp(op.Vote{Voter: "alice", Author: "bob", Permlink: "p2", Weight: 10000}); err != nil {
		t.Fatalf("vote 2: %v", err)
	}
	manaAfterVote2 := f.Account_("alice").VotingManaCurrent
	if manaAfterVote2 >= manaBeforeVote2 {
		t.Fatalf("vote 2 didn't drain mana: before=%d, after=%d", manaBeforeVote2, manaAfterVote2)
	}

	// Both votes should have generated rshares; vote 2's NetRshares
	// should be SMALLER than vote 1's because alice's mana was lower.
	c1 := f.Comment("bob", "p1")
	c2 := f.Comment("bob", "p2")
	if c1.NetRshares <= 0 || c2.NetRshares <= 0 {
		t.Fatalf("expected positive NetRshares, got p1=%d p2=%d", c1.NetRshares, c2.NetRshares)
	}
	if c2.NetRshares >= c1.NetRshares {
		t.Fatalf("vote 2 didn't drain mana between casts: p1.NetRshares=%d, p2.NetRshares=%d (expected p2 < p1)",
			c1.NetRshares, c2.NetRshares)
	}
}

// Mirrors the exact production sequence at craig-grant 3,540,652:
// voter votes, then creates her OWN comment, then votes on her own
// comment. The intervening Comment op runs PutAccount on the same
// account; if commentEvaluator's authorAcct load returns stale state
// (pre-first-vote), the PutAccount overwrites the mana drain and the
// second vote sees full mana.
func TestVoteApplyConsecutiveAroundOwnComment(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF12)
	f.Actor("alice")
	f.Actor("bob")
	f.FundVesting("alice", 100_000_000)
	f.GenerateBlock()

	// bob's root post (target of vote 1).
	if err := f.ApplyOp(op.Comment{Author: "bob", Permlink: "p1", Body: "hi"}); err != nil {
		t.Fatalf("bob comment: %v", err)
	}
	f.GenerateBlock()
	f.SkipSeconds(6 * 60)

	// Vote 1: alice votes on bob's post.
	if err := f.ApplyOp(op.Vote{Voter: "alice", Author: "bob", Permlink: "p1", Weight: 10000}); err != nil {
		t.Fatalf("vote 1: %v", err)
	}
	f.GenerateBlock()
	manaAfterVote1 := f.Account_("alice").VotingManaCurrent

	// Alice creates her own root post (a NEW one, not a reply, to keep
	// it simple; need to wait 5 min between root posts so simulate the
	// reply path by parenting to bob).
	if err := f.ApplyOp(op.Comment{Author: "alice", Permlink: "ap1", Body: "reply", ParentAuthor: "bob", ParentPermlink: "p1"}); err != nil {
		t.Fatalf("alice comment: %v", err)
	}
	f.GenerateBlock()
	manaAfterComment := f.Account_("alice").VotingManaCurrent
	if manaAfterComment != manaAfterVote1 {
		t.Fatalf("Comment op changed voter mana: vote1_after=%d, comment_after=%d (delta=%d)",
			manaAfterVote1, manaAfterComment, manaAfterComment-manaAfterVote1)
	}

	// Vote 2: alice votes on her own comment.
	if err := f.ApplyOp(op.Vote{Voter: "alice", Author: "alice", Permlink: "ap1", Weight: 10000}); err != nil {
		t.Fatalf("vote 2: %v", err)
	}
	manaAfterVote2 := f.Account_("alice").VotingManaCurrent
	if manaAfterVote2 >= manaAfterComment {
		t.Fatalf("vote 2 didn't drain mana: comment_after=%d, vote2_after=%d",
			manaAfterComment, manaAfterVote2)
	}

	// Vote 1 and vote 2's NetRshares should differ — vote 2 had less
	// mana, so should have lower NetRshares.
	c1 := f.Comment("bob", "p1")
	c2 := f.Comment("alice", "ap1")
	if c1.NetRshares <= 0 || c2.NetRshares <= 0 {
		t.Fatalf("expected positive NetRshares, got bob/p1=%d alice/ap1=%d", c1.NetRshares, c2.NetRshares)
	}
	if c2.NetRshares >= c1.NetRshares {
		t.Fatalf("second vote should have lower rshares: bob/p1=%d alice/ap1=%d (this is the suspected production bug)",
			c1.NetRshares, c2.NetRshares)
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
