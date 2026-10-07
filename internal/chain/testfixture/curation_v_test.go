package testfixture

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// TestCurationOneVotePerComment ports the spirit of curation_reward_tests.cpp::
// one_vote_for_comment: three parallel comments each receive a single
// equal-stake equal-percent vote. With identical stake/percent/timing on
// parallel posts (each voter starts from curve baseline 0), the
// curation weights should be identical.
func TestCurationOneVotePerComment(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF20)
	f.Actors("a0", "a1", "a2", "v0", "v1", "v2")
	f.FundVesting("v0", 1_000_000)
	f.FundVesting("v1", 1_000_000)
	f.FundVesting("v2", 1_000_000)
	f.GenerateBlock()

	// Three parallel posts.
	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "a0", Permlink: "p", Body: "x"})
	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "a1", Permlink: "p", Body: "x"})
	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "a2", Permlink: "p", Body: "x"})
	f.SkipSeconds(60 * 60)

	// One vote per comment, all in the same block.
	f.MustApplyOp(op.Vote{Voter: "v0", Author: "a0", Permlink: "p", Weight: 10000})
	f.MustApplyOp(op.Vote{Voter: "v1", Author: "a1", Permlink: "p", Weight: 10000})
	f.MustApplyOp(op.Vote{Voter: "v2", Author: "a2", Permlink: "p", Weight: 10000})

	cv0, _ := objects.GetCommentVote(state.NewSession(f.State), "v0", "a0", "p")
	cv1, _ := objects.GetCommentVote(state.NewSession(f.State), "v1", "a1", "p")
	cv2, _ := objects.GetCommentVote(state.NewSession(f.State), "v2", "a2", "p")

	if cv0.CurationWeight != cv1.CurationWeight || cv1.CurationWeight != cv2.CurationWeight {
		t.Errorf("curation weights should match: %d %d %d",
			cv0.CurationWeight, cv1.CurationWeight, cv2.CurationWeight)
	}
	if cv0.CurationWeight == 0 {
		t.Error("expected non-zero curation weight")
	}
}

// TestCurationTwoVotesForCommentDifferent ports the spirit of
// curation_reward_tests.cpp::two_votes_for_comment: two voters on the
// same post, the first vote receives MORE curation weight than the
// second under a square-root or quadratic curve (curve(rshares) - curve(0)
// > curve(2*rshares) - curve(rshares)).
func TestCurationTwoVotesForCommentDifferent(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF20)
	f.Actors("a0", "v0", "v1")
	f.FundVesting("v0", 1_000_000)
	f.FundVesting("v1", 1_000_000)
	f.GenerateBlock()

	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "a0", Permlink: "p", Body: "x"})
	f.SkipSeconds(60 * 60)

	f.MustApplyOp(op.Vote{Voter: "v0", Author: "a0", Permlink: "p", Weight: 10000})
	f.MustApplyOp(op.Vote{Voter: "v1", Author: "a0", Permlink: "p", Weight: 10000})

	cv0, _ := objects.GetCommentVote(state.NewSession(f.State), "v0", "a0", "p")
	cv1, _ := objects.GetCommentVote(state.NewSession(f.State), "v1", "a0", "p")
	// Convergent curves give the first voter MORE weight. The post-HF21
	// convergent_square_root curve makes the first vote dominate.
	if cv0.CurationWeight <= cv1.CurationWeight {
		t.Errorf("first voter should outweigh second on shared comment: v0=%d v1=%d",
			cv0.CurationWeight, cv1.CurationWeight)
	}
}

// TestCurationFiveVotersAllRewarded ports the spirit of
// curation_reward_tests.cpp::five_votes_per_comment: five voters all
// receive non-zero curation weight on the same comment.
func TestCurationFiveVotersAllRewarded(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF20)
	f.Actors("a0", "v1", "v2", "v3", "v4", "v5")
	for _, n := range []string{"v1", "v2", "v3", "v4", "v5"} {
		f.FundVesting(n, 1_000_000)
	}
	f.GenerateBlock()

	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "a0", Permlink: "p", Body: "x"})
	f.SkipSeconds(60 * 60)
	for _, n := range []string{"v1", "v2", "v3", "v4", "v5"} {
		f.MustApplyOp(op.Vote{Voter: n, Author: "a0", Permlink: "p", Weight: 10000})
	}

	for _, n := range []string{"v1", "v2", "v3", "v4", "v5"} {
		cv, err := objects.GetCommentVote(state.NewSession(f.State), n, "a0", "p")
		if err != nil {
			t.Fatalf("GetCommentVote %s: %v", n, err)
		}
		if cv.CurationWeight == 0 {
			t.Errorf("voter %s: expected non-zero curation weight", n)
		}
	}
}

// TestCurationTwoCommentsInSameBlock ports the spirit of
// curation_reward_tests.cpp::two_comments_in_the_same_blocks: two posts
// in the same block, each gets one vote from its own voter; both should
// produce non-zero, equal curation weights.
func TestCurationTwoCommentsInSameBlock(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF20)
	f.Actors("a0", "a1", "v0", "v1")
	f.FundVesting("v0", 1_000_000)
	f.FundVesting("v1", 1_000_000)
	f.GenerateBlock()

	// Two parallel posts in the same block.
	f.PushOp(op.Comment{ParentPermlink: "test", Author: "a0", Permlink: "p", Body: "x"})
	f.PushOp(op.Comment{ParentPermlink: "test", Author: "a1", Permlink: "p", Body: "x"})
	f.GenerateBlock()
	f.SkipSeconds(60 * 60)

	// One vote each.
	f.PushOp(op.Vote{Voter: "v0", Author: "a0", Permlink: "p", Weight: 10000})
	f.PushOp(op.Vote{Voter: "v1", Author: "a1", Permlink: "p", Weight: 10000})
	f.GenerateBlock()

	cv0, _ := objects.GetCommentVote(state.NewSession(f.State), "v0", "a0", "p")
	cv1, _ := objects.GetCommentVote(state.NewSession(f.State), "v1", "a1", "p")
	if cv0.CurationWeight == 0 || cv1.CurationWeight == 0 {
		t.Error("both voters should have non-zero curation weight")
	}
	if cv0.CurationWeight != cv1.CurationWeight {
		t.Errorf("parallel-comment same-block votes should produce equal weights: %d vs %d",
			cv0.CurationWeight, cv1.CurationWeight)
	}
}

// TestCurationTwoCommentsInDifferentBlocks ports the spirit of
// curation_reward_tests.cpp::two_comments_in_different_blocks: posts in
// different blocks, each voted by a different voter at the same time
// post-creation. Both should still produce equal curation weights since
// the per-comment timing is identical.
func TestCurationTwoCommentsInDifferentBlocks(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF20)
	f.Actors("a0", "a1", "v0", "v1")
	f.FundVesting("v0", 1_000_000)
	f.FundVesting("v1", 1_000_000)
	f.GenerateBlock()

	// First post.
	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "a0", Permlink: "p", Body: "x"})
	f.SkipSeconds(60 * 60)
	f.MustApplyOp(op.Vote{Voter: "v0", Author: "a0", Permlink: "p", Weight: 10000})

	// Second post several blocks later.
	f.GenerateBlocks(5)
	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "a1", Permlink: "p", Body: "x"})
	f.SkipSeconds(60 * 60)
	f.MustApplyOp(op.Vote{Voter: "v1", Author: "a1", Permlink: "p", Weight: 10000})

	cv0, _ := objects.GetCommentVote(state.NewSession(f.State), "v0", "a0", "p")
	cv1, _ := objects.GetCommentVote(state.NewSession(f.State), "v1", "a1", "p")
	if cv0.CurationWeight == 0 || cv1.CurationWeight == 0 {
		t.Error("both voters should have non-zero curation weight")
	}
	// With identical per-comment timing (1h post-creation each), the
	// weights match.
	if cv0.CurationWeight != cv1.CurationWeight {
		t.Errorf("different-block parallel votes with identical timing: %d vs %d",
			cv0.CurationWeight, cv1.CurationWeight)
	}
}
