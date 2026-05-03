package testfixture

import (
	"testing"

	"hived-go/internal/protocol/op"
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
		ProposalIDs:   []int64{1},
	})
	if err == nil {
		t.Fatal("expected rejection: non-owner cannot remove proposal")
	}
}
