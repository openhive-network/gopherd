package evaluator

import (
	"errors"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/network"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// The proposal-removal budget. Mainnet 200, testnet 20 — the tests below are
// written against the constant so they exercise the real boundary in either
// build.
var removeBudget = int(network.GlobalRemoveThreshold)

const proposalTestHeadTime uint32 = 1_600_000_000

// seedProposals builds a state with `owners` proposals (one per entry, ids
// 0..n-1 in the given order) and attaches `votes[i]` synthetic voters to
// proposal i.
//
// Votes are written directly rather than through update_proposal_votes: what
// is under test is the REMOVAL budget, and hundreds of real voters would only
// add account-creation noise to the setup.
func seedProposals(t *testing.T, owners []string, votes []int) *state.Session {
	t.Helper()
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := state.NewSession(db)
	for i, owner := range owners {
		objects.PutProposal(s, &objects.Proposal{
			ID:        int64(i),
			Creator:   auth.AccountName(owner),
			Receiver:  auth.AccountName(owner),
			StartDate: proposalTestHeadTime - 1000,
			// Far enough out that the proposal is NOT already due for the
			// sweep; only a removal can pull end_date back.
			EndDate:  proposalTestHeadTime + 10_000_000,
			DailyPay: 1000,
		})
		for v := 0; v < votes[i]; v++ {
			objects.PutProposalVote(s, int64(i), syntheticVoter(v))
		}
	}
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}
	return state.NewSession(db)
}

// syntheticVoter produces distinct, valid account names in ascending order so
// the (proposal_id, voter) iteration order is predictable.
func syntheticVoter(i int) auth.AccountName {
	const digits = "abcdefghij"
	return auth.AccountName("voter-" +
		string(digits[(i/100)%10]) + string(digits[(i/10)%10]) + string(digits[i%10]))
}

func countVotes(t *testing.T, s *state.Session, id int64) int {
	t.Helper()
	n := 0
	prefix := state.AppendInt64BE(nil, id)
	_ = s.Range(objects.ProposalVoteTable, state.PrimaryIndex, prefix,
		func(_, _ []byte) (bool, error) { n++; return true, nil })
	return n
}

func removeProposalOp(owner string, ids ...int64) op.RemoveProposal {
	return op.RemoveProposal{
		ProposalOwner: auth.AccountName(owner),
		ProposalIDs:   ids,
	}
}

// TestRemoveProposalBudgetFlagsRemainder pins the behaviour that makes HF29's
// VALIDATE_ALL_PROPOSAL_IDS necessary in the first place: a removal that runs
// out of budget cannot finish, so hived flags what is left instead of erasing
// it (dhf_evaluator.cpp:260).
//
// gopherd erased everything unconditionally before this, which looks harmless
// — the end state converges once the sweep runs — but it is the reason the
// ownership hole below was not reproducible here.
func TestRemoveProposalBudgetFlagsRemainder(t *testing.T) {
	// Exactly `budget` votes: the vote sweep spends the entire budget, so
	// there is nothing left to erase the proposal itself with.
	s := seedProposals(t, []string{"alice"}, []int{removeBudget})
	ctx := &Context{DB: s, Hardforks: preHF29Set(), HeadBlockTime: proposalTestHeadTime}

	if err := Apply(ctx, removeProposalOp("alice", 0)); err != nil {
		t.Fatalf("remove_proposal: %v", err)
	}

	p, err := objects.GetProposal(s, 0)
	if err != nil {
		t.Fatalf("proposal 0 should still exist (budget spent on its votes), got %v", err)
	}
	if !p.Removed {
		t.Error("proposal should be flagged removed")
	}
	want := proposalTestHeadTime - objects.ProposalMaintenanceCleanupSec
	if p.EndDate != want {
		t.Errorf("end_date: got %d, want %d (head_time - HIVE_PROPOSAL_MAINTENANCE_CLEANUP)", p.EndDate, want)
	}
	if n := countVotes(t, s, 0); n != 0 {
		t.Errorf("votes left: got %d, want 0 (the whole budget went on them)", n)
	}
}

// TestRemoveProposalWithinBudgetErasesIt is the other side of the same gate:
// under the budget, nothing is flagged and the row is gone.
func TestRemoveProposalWithinBudgetErasesIt(t *testing.T) {
	s := seedProposals(t, []string{"alice"}, []int{removeBudget - 1})
	ctx := &Context{DB: s, Hardforks: preHF29Set(), HeadBlockTime: proposalTestHeadTime}

	if err := Apply(ctx, removeProposalOp("alice", 0)); err != nil {
		t.Fatalf("remove_proposal: %v", err)
	}
	if has, _ := objects.HasProposal(s, 0); has {
		t.Error("proposal should be gone: votes + proposal fit inside the budget")
	}
	if n := countVotes(t, s, 0); n != 0 {
		t.Errorf("votes left: got %d, want 0", n)
	}
}

// TestRemoveProposalOwnerCheckSkippedPastBudget documents the hole HF29
// closes. Pre-HF29 the ownership assertion is conditional:
//
//	if( _db.has_hardfork( ..._VALIDATE_ALL_PROPOSAL_IDS ) || obj_perf.done() == false )
//	  FC_ASSERT( found_proposal->creator == op.proposal_owner, ... );
//
// so once the budget is spent, anyone's proposal named later in the same
// operation is accepted — and flagged, which pulls its end_date back and stops
// it being funded. That is a live griefing vector, and it is also a rule
// gopherd MUST honour while replaying: refusing the operation would wedge us
// on a block the network accepted.
func TestRemoveProposalOwnerCheckSkippedPastBudget(t *testing.T) {
	// ids are a flat_set on the wire — ascending — so alice's vote-heavy
	// proposal (id 0) is processed before bob's (id 1).
	s := seedProposals(t, []string{"alice", "bob"}, []int{removeBudget, 0})
	ctx := &Context{DB: s, Hardforks: preHF29Set(), HeadBlockTime: proposalTestHeadTime}

	if err := Apply(ctx, removeProposalOp("alice", 0, 1)); err != nil {
		t.Fatalf("pre-HF29 this must be ACCEPTED (hived accepts it): %v", err)
	}

	bobs, err := objects.GetProposal(s, 1)
	if err != nil {
		t.Fatalf("bob's proposal should still exist: %v", err)
	}
	if !bobs.Removed {
		t.Error("bob's proposal should have been flagged by alice's operation — that is the pre-HF29 hole")
	}
}

// TestRemoveProposalHF29EnforcesOwnerPastBudget is the same operation once
// HF29 is active: the ownership check no longer depends on the budget.
func TestRemoveProposalHF29EnforcesOwnerPastBudget(t *testing.T) {
	s := seedProposals(t, []string{"alice", "bob"}, []int{removeBudget, 0})
	ctx := &Context{DB: s, Hardforks: hf29Set(), HeadBlockTime: proposalTestHeadTime}

	err := Apply(ctx, removeProposalOp("alice", 0, 1))
	if !errors.Is(err, ErrProposalNotOwnedByYou) {
		t.Fatalf("post-HF29 must reject: got %v, want ErrProposalNotOwnedByYou", err)
	}
}

// TestRemoveProposalHF29RejectsAlreadyFlagged covers the other half of
// VALIDATE_ALL_PROPOSAL_IDS: a proposal already flagged for removal counts as
// nonexistent, so naming it again fails — hived's own test asserts exactly
// this (proposal_tests.cpp::proposals_removing_with_threshold_01, adjusted in
// ce4e5c6d3).
//
// Pre-HF29 the same operation is accepted and simply tries the removal again,
// which is how a flagged proposal can still be cleared by hand.
func TestRemoveProposalHF29RejectsAlreadyFlagged(t *testing.T) {
	seedFlagged := func(t *testing.T) *state.Session {
		t.Helper()
		s := seedProposals(t, []string{"alice"}, []int{removeBudget})
		ctx := &Context{DB: s, Hardforks: preHF29Set(), HeadBlockTime: proposalTestHeadTime}
		if err := Apply(ctx, removeProposalOp("alice", 0)); err != nil {
			t.Fatalf("setup removal: %v", err)
		}
		if p, err := objects.GetProposal(s, 0); err != nil || !p.Removed {
			t.Fatalf("setup should have left proposal 0 flagged (err=%v)", err)
		}
		return s
	}

	t.Run("post-HF29 rejects", func(t *testing.T) {
		s := seedFlagged(t)
		ctx := &Context{DB: s, Hardforks: hf29Set(), HeadBlockTime: proposalTestHeadTime}
		if err := Apply(ctx, removeProposalOp("alice", 0)); !errors.Is(err, ErrProposalNotFound) {
			t.Fatalf("got %v, want ErrProposalNotFound", err)
		}
	})

	t.Run("pre-HF29 retries the removal", func(t *testing.T) {
		s := seedFlagged(t)
		ctx := &Context{DB: s, Hardforks: preHF29Set(), HeadBlockTime: proposalTestHeadTime}
		if err := Apply(ctx, removeProposalOp("alice", 0)); err != nil {
			t.Fatalf("pre-HF29 must accept: %v", err)
		}
		// Fresh budget, no votes left: this time the row goes.
		if has, _ := objects.HasProposal(s, 0); has {
			t.Error("second removal should have erased the flagged proposal")
		}
	})
}
