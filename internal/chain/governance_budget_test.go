package chain

import (
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/network"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// TestClearProposalVotesForVoterCountsRemovals pins the counter the governance
// sweeps meter against hived's remove_guard.
//
// remove_expired_governance_votes and process_decline_voting_rights each open
// their own remove_guard( HIVE_GLOBAL_REMOVE_THRESHOLD ), counting
// proposal_vote removals, and defer an account (and every later one) to a
// later block when it runs out. gopherd clears every due account at once,
// which is consensus-visible whenever the budget is actually reached: witness
// votes would be cleared a block earlier than hived clears them.
//
// The port is blocked on account ids (hived's iteration order is
// (expiration_ts, account_id), and gopherd has no ids), so the budget is
// measured rather than enforced. This test is what makes the measurement
// trustworthy: a miscount would make a real divergence look clean.
func TestClearProposalVotesForVoterCountsRemovals(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := state.NewSession(db)

	// alice votes on 5 proposals, bob on 2, and a third voter's rows sit in
	// between so the scan has to filter rather than count everything.
	for i := 0; i < 5; i++ {
		objects.PutProposalVote(s, int64(i), auth.AccountName("alice"))
		objects.PutProposalVote(s, int64(i), auth.AccountName("carol"))
	}
	for i := 0; i < 2; i++ {
		objects.PutProposalVote(s, int64(100+i), auth.AccountName("bob"))
	}

	if got := clearProposalVotesForVoter(s, "alice"); got != 5 {
		t.Errorf("alice: got %d removals, want 5", got)
	}
	if got := clearProposalVotesForVoter(s, "bob"); got != 2 {
		t.Errorf("bob: got %d removals, want 2", got)
	}
	// Idempotent: nothing left to remove, nothing counted.
	if got := clearProposalVotesForVoter(s, "alice"); got != 0 {
		t.Errorf("alice second pass: got %d removals, want 0", got)
	}
	// carol's rows are untouched — the filter works.
	n := 0
	_ = s.Range(objects.ProposalVoteTable, state.PrimaryIndex, nil,
		func(_, _ []byte) (bool, error) { n++; return true, nil })
	if n != 5 {
		t.Errorf("rows left: got %d, want 5 (carol's)", n)
	}
}

// TestNoteGovernanceRemovalsFlagsOverBudget checks the threshold comparison
// itself: exactly at the limit is fine (hived's guard permits the last
// removal), one past it is the divergence.
func TestNoteGovernanceRemovalsFlagsOverBudget(t *testing.T) {
	limit := int(network.GlobalRemoveThreshold)

	beforeOver, _ := GovernanceRemovalStats()
	if noteGovernanceRemovals(0) {
		t.Error("a block that removed nothing must not be flagged")
	}
	if noteGovernanceRemovals(limit) {
		t.Errorf("%d removals is exactly the budget and must not be flagged", limit)
	}
	if after, _ := GovernanceRemovalStats(); after != beforeOver {
		t.Error("at-or-under-budget sweeps must not increment the over-budget count")
	}

	if !noteGovernanceRemovals(limit + 1) {
		t.Errorf("%d removals exceeds the %d budget and must be flagged", limit+1, limit)
	}
	after, maxPer := GovernanceRemovalStats()
	if after != beforeOver+1 {
		t.Errorf("over-budget count: got %d, want %d", after, beforeOver+1)
	}
	if maxPer < uint64(limit+1) {
		t.Errorf("high-water mark: got %d, want >= %d", maxPer, limit+1)
	}

	// The high-water mark only ever rises.
	_ = noteGovernanceRemovals(1)
	if _, m := GovernanceRemovalStats(); m != maxPer {
		t.Errorf("high-water mark regressed: got %d, want %d", m, maxPer)
	}
}
