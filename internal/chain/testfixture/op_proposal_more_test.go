package testfixture

import (
	"testing"

	"hived-go/internal/chain"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Additional create_proposal validation cases ported from
// proposal_tests.cpp::create_proposal_004 .. create_proposal_009.
//
// The Go DHF evaluator validates these structurally and via apply-side
// balance/permission checks; tests fail-fast at any negative path.

// TestCreateProposalEndDateInPast — proposal end_date must be in the future.
func TestCreateProposalEndDateInPast(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 80_000)
	f.GenerateBlock()

	now := f.HeadBlockTime()
	err := f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "bob",
		StartDate: now - 86400 - 60, // start 1 day ago
		EndDate:   now - 60,         // end in the recent past
		DailyPay:  hbdAsset(100),
		Subject:   "x", Permlink: "p",
	})
	if err == nil {
		t.Fatal("expected rejection for end_date in the past")
	}
}

// TestCreateProposalDailyPayWrongSymbol — daily_pay must be HBD.
func TestCreateProposalDailyPayWrongSymbol(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 80_000)
	f.GenerateBlock()

	now := f.HeadBlockTime()
	err := f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "bob",
		StartDate: now + 60,
		EndDate:   now + 86400,
		DailyPay:  HIVE(100), // HIVE not allowed
		Subject:   "x", Permlink: "p",
	})
	if err == nil {
		t.Fatal("expected rejection for non-HBD daily_pay")
	}
}

// TestCreateProposalDailyPayNegative — daily_pay must be non-negative.
func TestCreateProposalDailyPayNegative(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 80_000)
	f.GenerateBlock()

	now := f.HeadBlockTime()
	err := f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "bob",
		StartDate: now + 60, EndDate: now + 86400,
		DailyPay: hbdAsset(-1),
		Subject:  "x", Permlink: "p",
	})
	if err == nil {
		t.Fatal("expected rejection for negative daily_pay")
	}
}

// TestCreateProposalEmptySubject — subject must be non-empty.
func TestCreateProposalEmptySubject(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 80_000)
	f.GenerateBlock()

	now := f.HeadBlockTime()
	err := f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "bob",
		StartDate: now + 60, EndDate: now + 86400,
		DailyPay: hbdAsset(100),
		Subject:  "",
		Permlink: "p",
	})
	if err == nil {
		t.Fatal("expected rejection for empty subject")
	}
}

// TestCreateProposalInsufficientHBDForFee — proposal creation requires
// the creator to pay HIVE_TREASURY_FEE in HBD; without funds it fails.
func TestCreateProposalInsufficientHBDForFee(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	// No HBD funding for alice.
	f.GenerateBlock()

	now := f.HeadBlockTime()
	err := f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "bob",
		StartDate: now + 60, EndDate: now + 86400,
		DailyPay: hbdAsset(100),
		Subject:  "x", Permlink: "p",
	})
	if err == nil {
		t.Fatal("expected rejection: insufficient HBD for treasury fee")
	}
}

// TestUpdateProposalVotesNonemptyEmptyApproval — submitting a vote
// with no proposal_ids should be rejected (empty op).
func TestUpdateProposalVotesEmptyList(t *testing.T) {
	f := New(t)
	f.Actors("alice")
	f.GenerateBlock()

	err := f.ApplyOp(op.UpdateProposalVotes{
		Voter:       "alice",
		ProposalIDs: []int64{},
		Approve:     true,
	})
	if err == nil {
		t.Fatal("expected rejection for empty ProposalIDs list")
	}
}

// TestRemoveProposalEmptyList — submitting a removal with no ids is rejected.
func TestRemoveProposalEmptyList(t *testing.T) {
	f := New(t)
	f.Actors("alice")
	f.GenerateBlock()

	err := f.ApplyOp(op.RemoveProposal{
		ProposalOwner: "alice",
		ProposalIDs:   []int64{},
	})
	if err == nil {
		t.Fatal("expected rejection for empty RemoveProposal ids")
	}
}

// TestRemoveProposalNonOwnerRejected — only the proposal owner can
// remove their proposal. Mirrors proposal_tests.cpp::remove_proposal_005.
func TestRemoveProposalNonOwnerRejected(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 80_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF21)

	now := f.HeadBlockTime()
	if err := f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "bob",
		StartDate: now + 60, EndDate: now + 86400*7,
		DailyPay: hbdAsset(100),
		Subject:  "x", Permlink: "p",
	}); err != nil {
		t.Fatalf("create proposal: %v", err)
	}

	// Bob attempts to remove alice's proposal (id=1). Our evaluator
	// rejects if the proposal exists but is owned by someone else.
	err := f.ApplyOp(op.RemoveProposal{
		ProposalOwner: "bob",
		ProposalIDs:   []int64{0},
	})
	if err == nil {
		t.Fatal("expected rejection: non-owner cannot remove proposal")
	}
}

// TestProposalObjectApplyFeeIncrease ports proposal_tests.cpp::
// proposal_object_apply_fee_increase. A 90-day proposal duration
// triggers the HF24+ fee scaling: fee = HIVE_TREASURY_FEE +
// (90 - 60) * HIVE_PROPOSAL_FEE_INCREASE_AMOUNT
//        = 10_000 + 30 * 1_000 = 40_000 sat HBD.
//
// Assertions: creator's HBD drops by `fee`, receiver unchanged,
// treasury gains `fee`, the proposal_fee virtual op is emitted with
// matching (creator, treasury, fee), and the stored proposal carries
// the expected fields.
func TestProposalObjectApplyFeeIncrease(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF24)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 80_000)
	f.GenerateBlock()

	const (
		creator      = "alice"
		receiver     = "bob"
		treasuryName = "hive.fund"
		subject      = "hello"
		permlink     = "somethingpermlink"
	)

	const duration uint32 = 90
	const extraDays = duration - 60
	const fee int64 = 10_000 + int64(extraDays)*1_000 // = 40_000

	startDate := f.HeadBlockTime() + 86400
	endDate := startDate + duration*86400

	aliceBefore := f.HBDBalance(creator)
	bobBefore := f.HBDBalance(receiver)
	treasBefore := f.HBDBalance(treasuryName)

	if err := f.ApplyOp(op.CreateProposal{
		Creator:   creator,
		Receiver:  receiver,
		StartDate: startDate,
		EndDate:   endDate,
		DailyPay:  HBD(100),
		Subject:   subject,
		Permlink:  permlink,
	}); err != nil {
		t.Fatalf("create_proposal: %v", err)
	}

	// 1) creator paid fee
	if got, want := f.HBDBalance(creator), aliceBefore-fee; got != want {
		t.Errorf("creator HBD: got %d, want %d (diff %d)", got, want, got-want)
	}
	// 2) receiver unchanged
	if got := f.HBDBalance(receiver); got != bobBefore {
		t.Errorf("receiver HBD changed: got %d, want %d", got, bobBefore)
	}
	// 3) treasury credited fee
	if got, want := f.HBDBalance(treasuryName), treasBefore+fee; got != want {
		t.Errorf("treasury HBD: got %d, want %d (diff %d)", got, want, got-want)
	}

	// 4) proposal_fee virtual op was emitted with matching fields.
	vops := f.Chain.LastBlockVOps()
	var found *chain.BlockVOp
	for i := range vops {
		if vops[i].Kind == chain.BlockVOpProposalFee {
			found = &vops[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected proposal_fee vop; got %+v", vops)
	}
	if string(found.Creator) != creator {
		t.Errorf("vop.Creator: got %q, want %q", string(found.Creator), creator)
	}
	if string(found.Treasury) != treasuryName {
		t.Errorf("vop.Treasury: got %q, want %q", string(found.Treasury), treasuryName)
	}
	if found.Fee != fee {
		t.Errorf("vop.Fee: got %d, want %d", found.Fee, fee)
	}

	// 5) the stored proposal has all the expected fields.
	p, err := objects.GetProposal(state.NewSession(f.State), found.ProposalID)
	if err != nil {
		t.Fatalf("GetProposal(%d): %v", found.ProposalID, err)
	}
	if string(p.Creator) != creator {
		t.Errorf("proposal.Creator: got %q, want %q", string(p.Creator), creator)
	}
	if string(p.Receiver) != receiver {
		t.Errorf("proposal.Receiver: got %q, want %q", string(p.Receiver), receiver)
	}
	if p.StartDate != startDate {
		t.Errorf("proposal.StartDate: got %d, want %d", p.StartDate, startDate)
	}
	if p.EndDate != endDate {
		t.Errorf("proposal.EndDate: got %d, want %d", p.EndDate, endDate)
	}
	if p.DailyPay != 100 {
		t.Errorf("proposal.DailyPay: got %d, want 100", p.DailyPay)
	}
	if p.Subject != subject {
		t.Errorf("proposal.Subject: got %q, want %q", p.Subject, subject)
	}
	if p.Permlink != permlink {
		t.Errorf("proposal.Permlink: got %q, want %q", p.Permlink, permlink)
	}
}
