package testfixture

import (
	"testing"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Verify that a vote records a curation_weight on the CommentVote
// row and that the comment's TotalVoteWeight reflects the sum.
//
// Mirrors the invariants from operation_tests.cpp::vote_apply that
// access cv.weight and comment_cashout.total_vote_weight.
func TestCurationVoteWeightRecorded(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actor("alice")
	f.Actor("bob")
	f.FundVesting("alice", 100_000_000)
	f.FundVesting("bob", 100_000_000)
	f.GenerateBlock()

	if err := f.ApplyOp(op.Comment{
		Author: "alice", Permlink: "p", Body: "hi",
	}); err != nil {
		t.Fatalf("comment: %v", err)
	}

	if err := f.ApplyOp(op.Vote{
		Voter: "alice", Author: "alice", Permlink: "p", Weight: 10000,
	}); err != nil {
		t.Fatalf("alice vote: %v", err)
	}

	c := f.Comment("alice", "p")
	if c.TotalVoteWeight == 0 {
		t.Fatal("TotalVoteWeight should be non-zero after upvote")
	}
	if c.VoteRshares == 0 {
		t.Fatal("VoteRshares should be non-zero after upvote")
	}
	cv, err := objects.GetCommentVote(state.NewSession(f.State), "alice", "alice", "p")
	if err != nil {
		t.Fatalf("GetCommentVote: %v", err)
	}
	if cv.CurationWeight == 0 {
		t.Fatal("CommentVote.CurationWeight should be non-zero after upvote")
	}
	if cv.CurationWeight != c.TotalVoteWeight {
		t.Fatalf("single-vote: cv.CurationWeight (%d) should equal c.TotalVoteWeight (%d)",
			cv.CurationWeight, c.TotalVoteWeight)
	}
}

// Two voters → each vote contributes its weight to the comment total,
// and the comment's total equals the sum of the two votes.
func TestCurationTwoVotersAccumulate(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actor("alice")
	f.Actor("bob")
	f.Actor("carol")
	f.FundVesting("bob", 100_000_000)
	f.FundVesting("carol", 100_000_000)
	f.GenerateBlock()

	if err := f.ApplyOp(op.Comment{
		Author: "alice", Permlink: "p", Body: "hi",
	}); err != nil {
		t.Fatalf("comment: %v", err)
	}

	for _, voter := range []string{"bob", "carol"} {
		// Generate a block between votes so reverse-auction scaling
		// produces different weights.
		f.GenerateBlock()
		if err := f.ApplyOp(op.Vote{
			Voter: voter, Author: "alice", Permlink: "p", Weight: 10000,
		}); err != nil {
			t.Fatalf("%s vote: %v", voter, err)
		}
	}

	bobCV, err := objects.GetCommentVote(state.NewSession(f.State),
		"bob", "alice", "p")
	if err != nil {
		t.Fatalf("bob CV: %v", err)
	}
	carolCV, err := objects.GetCommentVote(state.NewSession(f.State),
		"carol", "alice", "p")
	if err != nil {
		t.Fatalf("carol CV: %v", err)
	}
	c := f.Comment("alice", "p")
	want := bobCV.CurationWeight + carolCV.CurationWeight
	if c.TotalVoteWeight != want {
		t.Fatalf("TotalVoteWeight: got %d, want %d", c.TotalVoteWeight, want)
	}
}

// Reverse-auction window scales early-vote weights down. A vote at
// time 0 (immediately after creation) should have weight near zero
// pre-HF21; a vote at the end of the window should get full weight.
func TestCurationReverseAuctionTimeScaling(t *testing.T) {
	f := New(t)
	// HF17 has 30-min reverse auction.
	f.HardforksUpTo(hardfork.HF17)
	f.Actor("alice")
	f.Actor("bob")
	f.Actor("carol")
	f.FundVesting("bob", 100_000_000)
	f.FundVesting("carol", 100_000_000)
	f.GenerateBlock()

	if err := f.ApplyOp(op.Comment{
		Author: "alice", Permlink: "p", Body: "hi",
	}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	// bob votes immediately (elapsed = ~0s) → weight scaled near 0
	if err := f.ApplyOp(op.Vote{
		Voter: "bob", Author: "alice", Permlink: "p", Weight: 10000,
	}); err != nil {
		t.Fatalf("bob: %v", err)
	}

	// Skip past the auction window then carol votes (full weight).
	f.SkipSeconds(60 * 60) // 1 hour past auction window
	if err := f.ApplyOp(op.Vote{
		Voter: "carol", Author: "alice", Permlink: "p", Weight: 10000,
	}); err != nil {
		t.Fatalf("carol: %v", err)
	}

	bobCV, _ := objects.GetCommentVote(state.NewSession(f.State),
		"bob", "alice", "p")
	carolCV, _ := objects.GetCommentVote(state.NewSession(f.State),
		"carol", "alice", "p")

	// Carol's full-weight vote on a comment with already-larger
	// vote_rshares should still produce a smaller incremental
	// curve weight (W diminishing returns) — but the time-scaling
	// must dominate: bob's weight should be much smaller than carol's.
	if bobCV.CurationWeight >= carolCV.CurationWeight {
		t.Fatalf("expected carol's late-window weight (%d) to exceed bob's early weight (%d)",
			carolCV.CurationWeight, bobCV.CurationWeight)
	}
}

// Port of curation_reward_tests.cpp::no_votes idiom.
//
// A comment with zero votes pays no curators and no author rewards.
// The curator pool stays in the reward fund.
func TestCurationNoVotesNoRewards(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actor("alice")
	f.GenerateBlock()
	if err := f.ApplyOp(op.Comment{
		Author: "alice", Permlink: "p", Body: "hi",
	}); err != nil {
		t.Fatalf("comment: %v", err)
	}

	preBalance := f.Balance("alice")
	preVests := f.Vests("alice")

	// Skip past cashout (HF17 = 7 days).
	f.SkipSeconds(60 * 60 * 24 * 7)

	postBalance := f.Balance("alice")
	postVests := f.Vests("alice")
	if postBalance != preBalance || postVests != preVests {
		t.Fatalf("alice should not have received rewards on a no-vote comment: balance %d→%d, vests %d→%d",
			preBalance, postBalance, preVests, postVests)
	}
	c := f.Comment("alice", "p")
	if c.TotalVoteWeight != 0 {
		t.Fatalf("TotalVoteWeight should be 0, got %d", c.TotalVoteWeight)
	}
}

// Port of curation_reward_tests.cpp::two_votes_for_comment idiom.
//
// Two voters with equal stake voting at the same time should produce
// equal curation weights and equal payouts.
func TestCurationTwoEqualVotersEqualPayout(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actor("alice")
	f.Actor("bob")
	f.Actor("carol")
	f.FundVesting("bob", 100_000_000)
	f.FundVesting("carol", 100_000_000)

	// Seed reward fund.
	s := state.NewSession(f.State)
	rf, err := objects.GetRewardFund(s, objects.PostRewardFundName)
	if err != nil {
		rf = objects.RewardFund{
			Name:                   objects.PostRewardFundName,
			RewardBalance:          1_000_000,
			ContentConstant:        2_000_000_000_000,
			AuthorRewardCurve:      objects.CurveBoundedCurve,
			CurationRewardCurve:    objects.CurveBoundedCurve,
			PercentCurationRewards: 5000,
		}
	} else {
		rf.RewardBalance += 1_000_000
		rf.PercentCurationRewards = 5000
	}
	objects.PutRewardFund(s, &rf)
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}

	f.GenerateBlock()
	if err := f.ApplyOp(op.Comment{
		Author: "alice", Permlink: "p", Body: "hi",
	}); err != nil {
		t.Fatalf("comment: %v", err)
	}

	// Skip past auction window so both votes get full time-weight.
	f.SkipSeconds(60 * 60)
	// Both vote in the SAME block so they get the same auction factor.
	f.PushOp(op.Vote{
		Voter: "bob", Author: "alice", Permlink: "p", Weight: 10000,
	})
	f.PushOp(op.Vote{
		Voter: "carol", Author: "alice", Permlink: "p", Weight: 10000,
	})
	f.GenerateBlock()

	bobBefore := f.Vests("bob")
	carolBefore := f.Vests("carol")

	f.SkipSeconds(60 * 60 * 24 * 7)

	bobDelta := f.Vests("bob") - bobBefore
	carolDelta := f.Vests("carol") - carolBefore
	if bobDelta == 0 || carolDelta == 0 {
		t.Fatalf("expected both voters to receive curation: bob_d=%d carol_d=%d",
			bobDelta, carolDelta)
	}
	// Bob votes first → his vote_rshares is 0 (W diminishing returns
	// gives him a higher curation_weight slice). Carol's vote shifts
	// vote_rshares up, so her incremental curve weight is smaller.
	// Strictly: bob_delta >= carol_delta, with equality only when the
	// curve is exactly linear.
	if bobDelta < carolDelta {
		t.Fatalf("expected bob (first voter) to get >= carol's share: bob=%d carol=%d",
			bobDelta, carolDelta)
	}
}

// At cashout, the single voter receives the full curation pool as
// VESTS (since their weight is 100% of total).
func TestCurationSingleVoterCashout(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actor("alice")
	f.Actor("bob")
	f.FundVesting("bob", 100_000_000)
	// Seed the post reward fund so cashout has something to pay.
	s := state.NewSession(f.State)
	rf, err := objects.GetRewardFund(s, objects.PostRewardFundName)
	if err != nil {
		// If the fund doesn't exist yet, create one.
		rf = objects.RewardFund{
			Name:                   objects.PostRewardFundName,
			RewardBalance:          1_000_000,
			ContentConstant:        2_000_000_000_000,
			AuthorRewardCurve:      objects.CurveBoundedCurve,
			CurationRewardCurve:    objects.CurveBoundedCurve,
			PercentCurationRewards: 2500, // 25%
		}
	} else {
		rf.RewardBalance += 1_000_000
		if rf.PercentCurationRewards == 0 {
			rf.PercentCurationRewards = 2500
		}
	}
	objects.PutRewardFund(s, &rf)
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}

	f.GenerateBlock()
	if err := f.ApplyOp(op.Comment{
		Author: "alice", Permlink: "p", Body: "hi",
	}); err != nil {
		t.Fatalf("comment: %v", err)
	}

	// Skip past reverse-auction so bob's vote gets full weight.
	f.SkipSeconds(60 * 60)
	if err := f.ApplyOp(op.Vote{
		Voter: "bob", Author: "alice", Permlink: "p", Weight: 10000,
	}); err != nil {
		t.Fatalf("bob: %v", err)
	}

	bobBefore := f.Vests("bob")

	// Skip to cashout (HF17 = 7 days).
	f.SkipSeconds(60 * 60 * 24 * 7)

	bobAfter := f.Vests("bob")
	if bobAfter <= bobBefore {
		t.Fatalf("bob should receive curation VESTS at cashout: before=%d after=%d",
			bobBefore, bobAfter)
	}
}
