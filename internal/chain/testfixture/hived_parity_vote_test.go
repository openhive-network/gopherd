package testfixture

import (
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// A9 — hive_evaluator_social.cpp:612-625. `allows_votes` is checked only
// while the comment_cashout object exists; after payout (HF19+ removes it)
// an upvote on a post that had votes disabled is a silent no-op, not an
// error. One such vote in a hived block used to make gopherd reject the
// whole block.
func TestVoteOnPaidOutCommentWithVotesDisabledIsNoOp(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF21)
	f.Actors("alice", "bob")
	f.FundVesting("alice", 10_000)
	f.FundVesting("bob", 10_000)
	f.GenerateBlock()
	for _, n := range []string{"w1", "w2", "w3", "w4", "w5", "w6", "w7"} {
		f.Actor(n)
		f.FundLiquid(n, 10_000)
		f.MustApplyOp(witnessUpdateOp(n))
	}
	for _, n := range []string{"w1", "w2", "w3", "w4", "w5", "w6", "w7"} {
		f.MustApplyOp(feedPublishOp(n, asset.Price{
			Base:  asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
			Quote: asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
		}))
	}

	f.MustApplyOp(op.Comment{Author: "alice", Permlink: "closed", ParentPermlink: "cat", Body: "body"})
	f.MustApplyOp(op.CommentOptions{
		Author: "alice", Permlink: "closed",
		MaxAcceptedPayout: HBD(1_000_000_000), PercentHbd: 10000,
		AllowVotes: false, AllowCurationRewards: true,
	})

	// While the cashout object exists, an upvote is rejected (both sides).
	if err := f.ApplyOp(op.Vote{Voter: "bob", Author: "alice", Permlink: "closed", Weight: 10000}); err == nil {
		t.Fatal("upvote on an unpaid comment with allow_votes=false was accepted")
	}

	// Past the HF17 cashout window the comment is paid and the cashout
	// object is gone.
	f.SkipSeconds(60*60*24*7 + 60)
	f.GenerateBlocks(3)
	c, err := objects.GetComment(state.NewSession(f.State), "alice", "closed")
	if err != nil {
		t.Fatalf("comment lookup: %v", err)
	}
	if !c.WasPaid {
		t.Fatalf("comment not paid out after the cashout window (cashout_time=%d, head=%d)", c.CashoutTime, f.HeadBlockTime())
	}

	// hived: `if( comment_cashout ) { allows_votes assert }` is skipped,
	// then `return; // comment already paid`. The transaction succeeds.
	if err := f.ApplyOp(op.Vote{Voter: "bob", Author: "alice", Permlink: "closed", Weight: 10000}); err != nil {
		t.Fatalf("upvote on a PAID comment with allow_votes=false rejected: %v (hived accepts it as a no-op)", err)
	}
}
