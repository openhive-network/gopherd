package testfixture

import (
	"testing"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// hbdAsset is a tiny helper that returns an HBD asset.
func hbdAsset(amount int64) asset.Asset {
	return asset.Asset{Amount: amount, Symbol: asset.Symbol{Num: asset.HbdSymbol}}
}

// TestCreateProposalInvalidCreator ports proposal_tests.cpp::create_proposal_001.
// Empty creator must be rejected.
func TestCreateProposalInvalidCreator(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()

	now := f.HeadBlockTime()
	err := f.ApplyOp(op.CreateProposal{
		Creator:   "",
		Receiver:  "bob",
		StartDate: now + 60,
		EndDate:   now + 60*60*24,
		DailyPay:  hbdAsset(100),
		Subject:   "test",
		Permlink:  "p",
	})
	if err == nil {
		t.Fatal("create_proposal with empty creator should fail")
	}
}

// TestCreateProposalInvalidReceiver ports create_proposal_002.
func TestCreateProposalInvalidReceiver(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()

	now := f.HeadBlockTime()
	err := f.ApplyOp(op.CreateProposal{
		Creator:   "alice",
		Receiver:  "",
		StartDate: now + 60,
		EndDate:   now + 60*60*24,
		DailyPay:  hbdAsset(100),
		Subject:   "test",
		Permlink:  "p",
	})
	if err == nil {
		t.Fatal("create_proposal with empty receiver should fail")
	}
}

// TestCreateProposalStartAfterEnd ports create_proposal_003 / 004.
// start_date > end_date must be rejected.
func TestCreateProposalStartAfterEnd(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()

	now := f.HeadBlockTime()
	err := f.ApplyOp(op.CreateProposal{
		Creator:   "alice",
		Receiver:  "bob",
		StartDate: now + 60*60*24*7, // 7 days from now
		EndDate:   now + 60*60*24,    // 1 day from now (< start)
		DailyPay:  hbdAsset(100),
		Subject:   "test",
		Permlink:  "p",
	})
	if err == nil {
		t.Fatal("create_proposal with start_date > end_date should fail")
	}
}

// TestCreateProposalSucceedsWithValidArgs is the positive baseline:
// well-formed args produce a Proposal row with a fresh ID.
func TestCreateProposalSucceedsWithValidArgs(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 80_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF21)

	now := f.HeadBlockTime()
	if err := f.ApplyOp(op.CreateProposal{
		Creator:   "alice",
		Receiver:  "bob",
		StartDate: now + 60,
		EndDate:   now + 60*60*24*30,
		DailyPay:  hbdAsset(100),
		Subject:   "test",
		Permlink:  "p",
	}); err != nil {
		t.Fatalf("create_proposal: %v", err)
	}

	// Walk proposals, expect at least one row.
	var found bool
	_ = state.NewSession(f.State).Range(objects.ProposalTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			p, err := objects.ReadProposal(value)
			if err == nil && p.Creator == "alice" && p.Receiver == "bob" {
				found = true
			}
			return true, nil
		})
	if !found {
		t.Error("created proposal not in ProposalTable")
	}
}

// TestUpdateProposalVotesAddRemove ports update_proposal_votes_apply.
// An approve=true creates a vote row and bumps TotalVotes; a follow-up
// approve=false removes it.
func TestUpdateProposalVotesAddRemove(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob", "carol")
	f.FundLiquidHBD("alice", 80_000)
	f.FundVesting("carol", 1_000_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF21)

	now := f.HeadBlockTime()
	f.MustApplyOp(op.CreateProposal{
		Creator:   "alice",
		Receiver:  "bob",
		StartDate: now + 60,
		EndDate:   now + 60*60*24*30,
		DailyPay:  hbdAsset(100),
		Subject:   "test",
		Permlink:  "p",
	})

	// Find the proposal ID (most recent, presumed only one).
	var pid int64 = -1
	_ = state.NewSession(f.State).Range(objects.ProposalTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			p, err := objects.ReadProposal(value)
			if err == nil && p.Creator == "alice" {
				pid = p.ID
			}
			return true, nil
		})
	if pid < 0 {
		t.Fatal("failed to locate proposal")
	}

	// carol approves.
	f.MustApplyOp(op.UpdateProposalVotes{
		Voter:       "carol",
		ProposalIDs: []int64{pid},
		Approve:     true,
	})
	if has, _ := objects.HasProposalVote(state.NewSession(f.State), pid, "carol"); !has {
		t.Error("carol's vote should be recorded")
	}
	p1, _ := objects.GetProposal(state.NewSession(f.State), pid)
	if p1.TotalVotes <= 0 {
		t.Errorf("after approve: TotalVotes=%d, want > 0", p1.TotalVotes)
	}

	// carol revokes.
	f.MustApplyOp(op.UpdateProposalVotes{
		Voter:       "carol",
		ProposalIDs: []int64{pid},
		Approve:     false,
	})
	if has, _ := objects.HasProposalVote(state.NewSession(f.State), pid, "carol"); has {
		t.Error("carol's vote should be removed after disapprove")
	}
}

// TestUpdateProposalVotesNonexistentProposal ports the spirit of
// update_proposal_votes_002: voting on a non-existent proposal id is
// rejected (pre-HF28). HF28+ silently skips per the C++
// `dont_try_vote_for_nonexistent_proposal` flag, but our replay window
// is pre-HF28.
func TestUpdateProposalVotesNonexistentProposal(t *testing.T) {
	f := New(t)
	f.Actors("alice")
	f.FundVesting("alice", 1_000_000)
	f.GenerateBlock()

	err := f.ApplyOp(op.UpdateProposalVotes{
		Voter:       "alice",
		ProposalIDs: []int64{99999}, // does not exist
		Approve:     true,
	})
	if err == nil {
		t.Error("update_proposal_votes for non-existent proposal should fail (pre-HF28)")
	}
}

// TestRemoveProposalDeletesRow ports remove_proposal_apply. A proposal
// owner removing their proposal removes it from the table.
func TestRemoveProposalDeletesRow(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 80_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF21)

	now := f.HeadBlockTime()
	f.MustApplyOp(op.CreateProposal{
		Creator:   "alice",
		Receiver:  "bob",
		StartDate: now + 60,
		EndDate:   now + 60*60*24*30,
		DailyPay:  hbdAsset(100),
		Subject:   "test",
		Permlink:  "p",
	})

	var pid int64 = -1
	_ = state.NewSession(f.State).Range(objects.ProposalTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			p, _ := objects.ReadProposal(value)
			if p.Creator == "alice" {
				pid = p.ID
			}
			return true, nil
		})
	if pid < 0 {
		t.Fatal("failed to locate proposal")
	}

	f.MustApplyOp(op.RemoveProposal{
		ProposalOwner: "alice",
		ProposalIDs:   []int64{pid},
	})

	// Proposal should be gone (we don't model a Removed flag — DELETE
	// strikes the row directly).
	if has, _ := objects.HasProposal(state.NewSession(f.State), pid); has {
		t.Errorf("proposal %d should be removed", pid)
	}
}
