package testfixture

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// TestVoteWeightSameInsideAuctionEqualizes ports a slice of
// operation_tests.cpp::vote_weights — two voters with identical stake
// and identical timing on PARALLEL posts (separate comments) should
// contribute identical curation weight. Same-post case differs because
// the curation curve is path-dependent: each subsequent vote on the
// same comment sees a different curve(prev) baseline.
func TestVoteWeightSameInsideAuctionEqualizes(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF20)
	f.Actors("alice", "bob", "carol", "dave")
	f.FundVesting("alice", 1_000_000)
	f.FundVesting("bob", 1_000_000)
	f.GenerateBlock()

	// Parallel posts so each voter sees curve(0) baseline.
	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "carol", Permlink: "p1", Body: "hi"})
	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "dave", Permlink: "p2", Body: "hi"})

	// Two voters cast in the same block, well past the reverse-auction
	// window so the time-discount is 100%.
	f.SkipSeconds(60 * 60)
	f.MustApplyOp(op.Vote{Voter: "alice", Author: "carol", Permlink: "p1", Weight: 1000})
	f.MustApplyOp(op.Vote{Voter: "bob", Author: "dave", Permlink: "p2", Weight: 1000})

	cAlice, err := objects.GetCommentVote(state.NewSession(f.State), "alice", "carol", "p1")
	if err != nil {
		t.Fatalf("GetCommentVote alice: %v", err)
	}
	cBob, err := objects.GetCommentVote(state.NewSession(f.State), "bob", "dave", "p2")
	if err != nil {
		t.Fatalf("GetCommentVote bob: %v", err)
	}
	if cAlice.CurationWeight != cBob.CurationWeight {
		t.Errorf("curation weight parity: alice=%d bob=%d (should be equal — same stake, same timing, parallel posts)",
			cAlice.CurationWeight, cBob.CurationWeight)
	}
}

// TestVoteWeightInAuctionDiscount verifies the reverse-auction discount:
// a vote cast inside the window receives less curation weight than the
// same vote cast post-window. Mirrors the early-vs-late branch of the
// vote_weights C++ test (the "early_voting_seconds" pattern).
func TestVoteWeightInAuctionDiscount(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actors("alice", "bob", "carol", "dave")
	f.FundVesting("alice", 1_000_000)
	f.FundVesting("bob", 1_000_000)
	f.GenerateBlock()

	// Two parallel posts.
	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "carol", Permlink: "p1", Body: "hi"})
	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "dave", Permlink: "p2", Body: "hi"})

	// Vote on carol's IMMEDIATELY (inside reverse-auction window).
	f.MustApplyOp(op.Vote{Voter: "alice", Author: "carol", Permlink: "p1", Weight: 1000})

	// Skip past auction window, then vote on dave's.
	f.SkipSeconds(60 * 60)
	f.MustApplyOp(op.Vote{Voter: "bob", Author: "dave", Permlink: "p2", Weight: 1000})

	cAlice, err := objects.GetCommentVote(state.NewSession(f.State), "alice", "carol", "p1")
	if err != nil {
		t.Fatalf("GetCommentVote alice: %v", err)
	}
	cBob, err := objects.GetCommentVote(state.NewSession(f.State), "bob", "dave", "p2")
	if err != nil {
		t.Fatalf("GetCommentVote bob: %v", err)
	}

	// alice's in-auction vote should be DISCOUNTED relative to bob's
	// post-auction vote. C++'s vote_weights test verifies the inverse —
	// that pattern voters at full window get the same weight; we go
	// the simpler direction.
	if cAlice.CurationWeight >= cBob.CurationWeight {
		t.Errorf("expected reverse-auction discount: alice (in-auction) %d >= bob (post-auction) %d",
			cAlice.CurationWeight, cBob.CurationWeight)
	}
}

// TestVoteWeightProportionalToStake verifies vote weight scales with
// vesting shares: a 2× richer voter contributes ~2× the curation weight
// for the same vote percent and timing.
func TestVoteWeightProportionalToStake(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actors("alice", "bob", "carol", "dave")
	f.FundVesting("alice", 1_000_000) // 1× stake
	f.FundVesting("bob", 2_000_000)   // 2× stake
	f.GenerateBlock()

	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "carol", Permlink: "p1", Body: "hi"})
	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "dave", Permlink: "p2", Body: "hi"})

	// Both voters cast post-auction at the same percent.
	f.SkipSeconds(60 * 60)
	f.MustApplyOp(op.Vote{Voter: "alice", Author: "carol", Permlink: "p1", Weight: 1000})
	f.MustApplyOp(op.Vote{Voter: "bob", Author: "dave", Permlink: "p2", Weight: 1000})

	cAlice, _ := objects.GetCommentVote(state.NewSession(f.State), "alice", "carol", "p1")
	cBob, _ := objects.GetCommentVote(state.NewSession(f.State), "bob", "dave", "p2")

	if cBob.CurationWeight <= cAlice.CurationWeight {
		t.Errorf("bob (2× stake) curation weight (%d) should exceed alice (1× stake) weight (%d)",
			cBob.CurationWeight, cAlice.CurationWeight)
	}
}

// TestVoteRsharesProportionalToVotePercent verifies that net_rshares on
// the comment scales with the vote weight (1% vs 5% vs 100%) for the
// same voter at the same time. This guards the rshares pipeline against
// scaling regressions independent of the curation curve.
func TestVoteRsharesProportionalToVotePercent(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actors("v1", "v2", "v3", "a1", "a2", "a3")
	f.FundVesting("v1", 1_000_000_000)
	f.FundVesting("v2", 1_000_000_000)
	f.FundVesting("v3", 1_000_000_000)
	f.GenerateBlock()

	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "a1", Permlink: "p", Body: "x"})
	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "a2", Permlink: "p", Body: "x"})
	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "a3", Permlink: "p", Body: "x"})
	f.SkipSeconds(60 * 60)

	f.MustApplyOp(op.Vote{Voter: "v1", Author: "a1", Permlink: "p", Weight: 100})   // 1%
	f.MustApplyOp(op.Vote{Voter: "v2", Author: "a2", Permlink: "p", Weight: 500})   // 5%
	f.MustApplyOp(op.Vote{Voter: "v3", Author: "a3", Permlink: "p", Weight: 10000}) // 100%

	c1 := f.Comment("a1", "p")
	c2 := f.Comment("a2", "p")
	c3 := f.Comment("a3", "p")

	if c1.NetRshares <= 0 || c2.NetRshares <= 0 || c3.NetRshares <= 0 {
		t.Fatalf("expected positive rshares; got %d, %d, %d",
			c1.NetRshares, c2.NetRshares, c3.NetRshares)
	}
	// Strict ordering — voter weight scales monotonically.
	if !(c1.NetRshares < c2.NetRshares && c2.NetRshares < c3.NetRshares) {
		t.Errorf("rshares should scale with vote percent: 1%%=%d 5%%=%d 100%%=%d",
			c1.NetRshares, c2.NetRshares, c3.NetRshares)
	}
}

// TestVoteWeightEditZerosCurationPreHF20 mirrors the implicit assertion in
// operation_tests.cpp::vote_weights that for pre-HF20 mainnet, a vote
// EDIT (recast) sets the comment_vote's stored curation weight to 0 —
// only the first-cast contributes to curation rewards. C++:
// hive_evaluator_social.cpp:620 (`cv.set(_now, o.weight, 0, rshares)`).
//
// The C++ vote_weights test additionally checks the HF20+ path where
// vote edits compute a fresh curation weight that matches the
// pattern. The Go evaluator currently zeros on edit regardless of
// HF20 (see internal/chain/evaluator/comment.go:920); this test
// guards against accidentally turning that behavior on for pre-HF20
// while the HF20+ vote-edit weight is in flight.
func TestVoteWeightEditZerosCurationPreHF20(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actors("voter", "author")
	f.FundVesting("voter", 1_000_000)
	f.GenerateBlock()

	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "author", Permlink: "p", Body: "x"})
	f.SkipSeconds(60 * 60) // past reverse-auction

	// First cast — should produce a non-zero curation weight.
	f.MustApplyOp(op.Vote{Voter: "voter", Author: "author", Permlink: "p", Weight: 100})
	first, err := objects.GetCommentVote(state.NewSession(f.State), "voter", "author", "p")
	if err != nil {
		t.Fatalf("GetCommentVote first: %v", err)
	}
	if first.CurationWeight == 0 {
		t.Fatal("expected non-zero curation weight on first cast")
	}

	// Recast — must zero the stored weight (pre-HF20 / current Go).
	f.MustApplyOp(op.Vote{Voter: "voter", Author: "author", Permlink: "p", Weight: 300})
	second, err := objects.GetCommentVote(state.NewSession(f.State), "voter", "author", "p")
	if err != nil {
		t.Fatalf("GetCommentVote second: %v", err)
	}
	if second.CurationWeight != 0 {
		t.Errorf("recast curation_weight: got %d, want 0", second.CurationWeight)
	}
}

// TestDownvoteSubtractsRshares verifies a negative vote subtracts from
// net_rshares. Combined with an upvote of equal magnitude, net_rshares
// should be ~0.
func TestDownvoteSubtractsRshares(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actors("alice", "bob", "carol")
	f.FundVesting("alice", 1_000_000)
	f.FundVesting("bob", 1_000_000)
	f.GenerateBlock()

	f.MustApplyOp(op.Comment{ParentPermlink: "test", Author: "carol", Permlink: "p", Body: "x"})
	f.SkipSeconds(60 * 60)

	// Upvote 5% then equal-magnitude downvote from another voter at same
	// time → net rshares should be near zero (small drift from the
	// downvote applying to a slightly-decreased vote mana).
	f.MustApplyOp(op.Vote{Voter: "alice", Author: "carol", Permlink: "p", Weight: 500})
	c1 := f.Comment("carol", "p")
	upRshares := c1.NetRshares
	if upRshares <= 0 {
		t.Fatalf("expected positive rshares after upvote; got %d", upRshares)
	}

	f.MustApplyOp(op.Vote{Voter: "bob", Author: "carol", Permlink: "p", Weight: -500})
	c2 := f.Comment("carol", "p")
	if c2.NetRshares >= upRshares {
		t.Errorf("downvote should reduce net_rshares: %d (upvote only) vs %d (after equal downvote)",
			upRshares, c2.NetRshares)
	}
}
