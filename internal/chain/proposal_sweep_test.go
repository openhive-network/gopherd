package chain

import (
	"fmt"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/network"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

const sweepHeadTime uint32 = 1_600_000_000

// TestRemoveOldProposalsSweep covers dhf_processor::remove_proposals
// (libraries/chain/util/dhf_processor.cpp:44), the per-block pass that
// finishes what a budget-limited remove_proposal_operation could not.
//
// Two rules are load-bearing and easy to get backwards:
//
//   - ONLY flagged proposals are swept. An expired proposal stays in state
//     forever on purpose, so list_proposals can still report it — gopherd
//     deleted expired proposals here once, which orphaned later unvotes.
//   - The sweep has the SAME budget as the operation, so clearing a
//     vote-heavy proposal takes several blocks.
func TestRemoveOldProposalsSweep(t *testing.T) {
	budget := int(network.GlobalRemoveThreshold)

	open := func(t *testing.T) *state.Session {
		t.Helper()
		db, err := state.Open(t.TempDir(), 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return state.NewSession(db)
	}
	votesFor := func(s *state.Session, id int64, n int) {
		for i := 0; i < n; i++ {
			objects.PutProposalVote(s, id, auth.AccountName(fmt.Sprintf("voter%04d", i)))
		}
	}
	countVotes := func(s *state.Session, id int64) int {
		n := 0
		_ = s.Range(objects.ProposalVoteTable, state.PrimaryIndex, state.AppendInt64BE(nil, id),
			func(_, _ []byte) (bool, error) { n++; return true, nil })
		return n
	}

	t.Run("flagged proposal is cleared over successive blocks", func(t *testing.T) {
		s := open(t)
		// Flagged exactly as the evaluator leaves it: end_date pulled back by
		// HIVE_PROPOSAL_MAINTENANCE_CLEANUP so get_end_date_with_delay() is
		// already at head time and the proposal is due on this very block.
		objects.PutProposal(s, &objects.Proposal{
			ID: 1, Creator: "alice", Receiver: "alice",
			StartDate: sweepHeadTime - 100_000,
			EndDate:   sweepHeadTime - objects.ProposalMaintenanceCleanupSec,
			Removed:   true,
		})
		votesFor(s, 1, budget+5)

		removeOldProposals(s, sweepHeadTime)
		if has, _ := objects.HasProposal(s, 1); !has {
			t.Fatal("first pass should have spent its whole budget on votes, leaving the proposal")
		}
		if got := countVotes(s, 1); got != 5 {
			t.Fatalf("votes after first pass: got %d, want 5", got)
		}

		removeOldProposals(s, sweepHeadTime+3)
		if has, _ := objects.HasProposal(s, 1); has {
			t.Error("second pass should have erased the proposal")
		}
		if got := countVotes(s, 1); got != 0 {
			t.Errorf("votes after second pass: got %d, want 0", got)
		}
	})

	t.Run("expired but unflagged proposals are left alone", func(t *testing.T) {
		s := open(t)
		objects.PutProposal(s, &objects.Proposal{
			ID: 2, Creator: "alice", Receiver: "alice",
			StartDate: sweepHeadTime - 100_000,
			EndDate:   sweepHeadTime - 100_000, // long expired, never flagged
		})
		votesFor(s, 2, 3)

		removeOldProposals(s, sweepHeadTime)

		if has, _ := objects.HasProposal(s, 2); !has {
			t.Error("an expired proposal must survive: only remove_proposal_operation retires one")
		}
		if got := countVotes(s, 2); got != 3 {
			t.Errorf("votes: got %d, want 3 (untouched)", got)
		}
	})

	t.Run("a flagged proposal not yet past the cleanup delay waits", func(t *testing.T) {
		s := open(t)
		// end_date in the future: get_end_date_with_delay() is past head time,
		// so upper_bound(head_time) does not reach it. Only reachable by
		// seeding state directly — the evaluator always pulls end_date back —
		// but it pins the boundary the index scan stops at.
		objects.PutProposal(s, &objects.Proposal{
			ID: 3, Creator: "alice", Receiver: "alice",
			StartDate: sweepHeadTime - 100_000,
			EndDate:   sweepHeadTime + 10,
			Removed:   true,
		})

		removeOldProposals(s, sweepHeadTime)
		if has, _ := objects.HasProposal(s, 3); !has {
			t.Error("flagged proposal swept before its delayed end date")
		}
	})
}
