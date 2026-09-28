package testfixture

import (
	"testing"

	"gopherd/internal/chain"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
)

// The virtual-op stream is ORDERED, and the order is load-bearing: a consumer
// that rebuilds balances by replaying it gets a different answer if two ops
// swap. hived's cashout emits curators first (so authors and beneficiaries see
// the post-curator vesting share price), then beneficiaries, then the author,
// then the comment-level summary, then the settled marker. Nothing sorts the
// buffer afterwards, so that order exists only because the pushes happen in it
// — which is exactly the kind of property that a refactor silently inverts.
//
// These tests also check the one cross-op invariant the payloads carry:
// author_reward's curators_vesting_payout is supposed to save a consumer from
// summing every curation_reward, so it had better equal that sum.

// vopIndex returns the position of the first op with the given tag, or -1.
func vopIndex(vops []op.Operation, tag op.Tag) int {
	for i, v := range vops {
		if v.OpTag() == tag {
			return i
		}
	}
	return -1
}

// generateUntilVOp rolls blocks forward until one carries the given tag,
// returning that block's stream. Which block a cashout lands in depends on the
// schedule, so searching for it beats hard-coding an offset that quietly stops
// matching.
func generateUntilVOp(t *testing.T, f *Fixture, tag op.Tag, maxBlocks int) []op.Operation {
	t.Helper()
	for i := 0; i < maxBlocks; i++ {
		f.GenerateBlock()
		vops := f.Chain.LastBlockVOps()
		if vopIndex(vops, tag) >= 0 {
			return vops
		}
	}
	t.Fatalf("no block in %d carried tag %d (%s)", maxBlocks, tag, tag.Name())
	return nil
}

func TestVOpCashoutStreamOrder(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actors("alice", "bob", "dave")
	f.FundVesting("alice", 100_000)
	f.FundVesting("bob", 100_000)
	f.FundVesting("dave", 10_000)
	f.GenerateBlock()

	f.MustApplyOp(op.Comment{Author: "alice", Permlink: "post", Body: "body"})
	// dave takes a quarter off the top of the author's share, so the stream
	// has to contain a benefactor op between the curators and the author.
	f.MustApplyOp(op.CommentOptions{
		Author: "alice", Permlink: "post",
		MaxAcceptedPayout:    HBD(1_000_000_000),
		PercentHbd:           10000,
		AllowVotes:           true,
		AllowCurationRewards: true,
		Beneficiaries:        []op.CommentBeneficiary{{Account: "dave", Weight: 2500}},
	})
	// Past the reverse auction so the curation weight is not discounted to
	// nothing — a zero-weight curator is paid zero and emits no op, which
	// would make this test pass for the wrong reason.
	f.SkipSeconds(60 * 60)
	f.MustApplyOp(op.Vote{Voter: "bob", Author: "alice", Permlink: "post", Weight: 10000})

	f.SkipSeconds(60 * 60 * 24 * 7)
	vops := generateUntilVOp(t, f, op.TagAuthorReward, 8)

	iCuration := vopIndex(vops, op.TagCurationReward)
	iBenefactor := vopIndex(vops, op.TagCommentBenefactorReward)
	iAuthor := vopIndex(vops, op.TagAuthorReward)
	iReward := vopIndex(vops, op.TagCommentReward)
	iUpdate := vopIndex(vops, op.TagCommentPayoutUpdate)

	for _, c := range []struct {
		name string
		idx  int
	}{
		{"curation_reward", iCuration},
		{"comment_benefactor_reward", iBenefactor},
		{"author_reward", iAuthor},
		{"comment_reward", iReward},
		{"comment_payout_update", iUpdate},
	} {
		if c.idx < 0 {
			t.Errorf("%s missing from the cashout stream", c.name)
		}
	}
	if t.Failed() {
		for i, v := range vops {
			t.Logf("  [%d] %s", i, v.OpTag().Name())
		}
		t.FailNow()
	}

	// hived's order (database_comment.cpp:259-363): curators, then
	// beneficiaries, then author, then the comment summary, then the settled
	// marker. Each create_vesting mutates the share price, so paying
	// beneficiaries before curators changes the amounts — this is a
	// consensus-visible order, not a cosmetic one.
	if !(iCuration < iBenefactor && iBenefactor < iAuthor && iAuthor < iReward && iReward < iUpdate) {
		t.Errorf("cashout stream out of order: curation=%d benefactor=%d author=%d reward=%d update=%d",
			iCuration, iBenefactor, iAuthor, iReward, iUpdate)
		for i, v := range vops {
			t.Logf("  [%d] %s", i, v.OpTag().Name())
		}
	}

	// curators_vesting_payout exists so a consumer can account the whole
	// claim from author_reward alone. If it does not equal the sum of the
	// curation_reward ops in the same block, it is worse than absent.
	var curatorSum int64
	for _, v := range vops {
		if cr, ok := v.(op.CurationReward); ok {
			curatorSum += cr.Reward.Amount
		}
	}
	ar, ok := vops[iAuthor].(op.AuthorReward)
	if !ok {
		t.Fatalf("author_reward slot holds %T", vops[iAuthor])
	}
	if ar.CuratorsVestingPayout.Amount != curatorSum {
		t.Errorf("author_reward.curators_vesting_payout = %d, want %d (sum of curation_reward)",
			ar.CuratorsVestingPayout.Amount, curatorSum)
	}
	if ar.Author != "alice" || ar.Permlink != "post" {
		t.Errorf("author_reward names %s/%s, want alice/post", ar.Author, ar.Permlink)
	}
	// HF17 routes author and curator payouts to the claimable reward
	// balances, and the flag is what tells a consumer the money is not
	// spendable yet.
	if !ar.PayoutMustBeClaimed {
		t.Error("author_reward.payout_must_be_claimed is false under HF17")
	}

	bo, ok := vops[iBenefactor].(op.CommentBenefactorReward)
	if !ok {
		t.Fatalf("benefactor slot holds %T", vops[iBenefactor])
	}
	if bo.Benefactor != "dave" || bo.Author != "alice" {
		t.Errorf("benefactor op names %s from %s, want dave from alice", bo.Benefactor, bo.Author)
	}
	// Pre-HF20 a beneficiary's whole slice is VESTS — the HBD leg only
	// appears from HF20 on, so a non-zero one here would mean the split ran
	// too early.
	if bo.HbdPayout.Amount != 0 {
		t.Errorf("benefactor hbd_payout = %d under HF17, want 0 (the split starts at HF20)", bo.HbdPayout.Amount)
	}
	if bo.VestingPayout.Amount <= 0 {
		t.Errorf("benefactor vesting_payout = %d, want > 0", bo.VestingPayout.Amount)
	}
}

// TestVOpEffectiveCommentVote pins the two things about this op that a naive
// wiring gets wrong: it reports the weight STORED on the row, and it is absent
// when the vote did not count.
func TestVOpEffectiveCommentVote(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actors("alice", "bob")
	f.FundVesting("alice", 100_000)
	f.FundVesting("bob", 100_000)
	f.GenerateBlock()

	f.MustApplyOp(op.Comment{Author: "alice", Permlink: "post", Body: "body"})
	f.SkipSeconds(60 * 60)
	f.MustApplyOp(op.Vote{Voter: "bob", Author: "alice", Permlink: "post", Weight: 10000})
	f.GenerateBlock()

	vops := f.Chain.LastBlockVOps()
	v := chain.FindVOp(vops, op.TagEffectiveCommentVote)
	if v == nil {
		t.Fatal("no effective_comment_vote for a vote that counted")
	}
	ev, ok := v.(op.EffectiveCommentVote)
	if !ok {
		t.Fatalf("effective_comment_vote slot holds %T", v)
	}
	if ev.Voter != "bob" || ev.Author != "alice" || ev.Permlink != "post" {
		t.Errorf("op names %s -> %s/%s, want bob -> alice/post", ev.Voter, ev.Author, ev.Permlink)
	}
	if ev.Rshares <= 0 {
		t.Errorf("rshares = %d, want > 0", ev.Rshares)
	}
	// The weight reported has to be the one on the comment_vote row, and the
	// comment's running total has to already include it: hived pushes this
	// after the accumulation, not before.
	c := f.Comment("alice", "post")
	if ev.TotalVoteWeight != c.TotalVoteWeight {
		t.Errorf("total_vote_weight = %d, want the comment's %d", ev.TotalVoteWeight, c.TotalVoteWeight)
	}
	if ev.Weight == 0 {
		t.Error("weight = 0 for a fresh full-power vote past the reverse auction")
	}
	// A consensus node never fills pending_payout; the account-history
	// plugin does. Emitting an estimate would put a number on the wire that
	// hived does not.
	if ev.PendingPayout.Amount != 0 {
		t.Errorf("pending_payout = %d, want 0 (AH plugin fills this, not consensus)", ev.PendingPayout.Amount)
	}
}
