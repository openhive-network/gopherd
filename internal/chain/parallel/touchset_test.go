package parallel

import (
	"testing"

	"hived-go/internal/protocol/op"
)

func TestAccountSetAddDedup(t *testing.T) {
	var s AccountSet
	s.Add("alice")
	s.Add("bob")
	s.Add("alice") // duplicate
	if s.Len() != 2 {
		t.Fatalf("len = %d, want 2", s.Len())
	}
	if !s.Contains("alice") || !s.Contains("bob") {
		t.Fatalf("missing membership: alice=%v bob=%v", s.Contains("alice"), s.Contains("bob"))
	}
	if s.Contains("carol") {
		t.Fatalf("carol shouldn't be present")
	}
}

func TestAccountSetOverflow(t *testing.T) {
	var s AccountSet
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	for _, n := range names {
		s.Add(n)
		s.Add(n) // dedup must hold across inline + overflow
	}
	if s.Len() != len(names) {
		t.Fatalf("len = %d, want %d", s.Len(), len(names))
	}
	for _, n := range names {
		if !s.Contains(n) {
			t.Fatalf("missing %q", n)
		}
	}
}

func TestAccountSetIntersects(t *testing.T) {
	var a, b AccountSet
	a.Add("alice")
	a.Add("bob")
	b.Add("carol")
	b.Add("dave")
	if a.Intersects(&b) {
		t.Fatalf("disjoint sets reported as intersecting")
	}
	b.Add("alice")
	if !a.Intersects(&b) {
		t.Fatalf("intersecting sets reported as disjoint")
	}
}

func TestTouchSetConflict_TransfersSameAccount(t *testing.T) {
	t1 := TouchSet{}
	Extract(&t1, op.Transfer{From: "alice", To: "bob"})
	t2 := TouchSet{}
	Extract(&t2, op.Transfer{From: "alice", To: "carol"})
	// Both transfers touch alice (and DGPO funds) — must conflict.
	if !t1.ConflictsWith(&t2) {
		t.Fatalf("transfers from same account should conflict")
	}
}

func TestTouchSetConflict_VotesAlwaysConflict(t *testing.T) {
	t1 := TouchSet{}
	Extract(&t1, op.Vote{Voter: "alice", Author: "a", Permlink: "p1"})
	t2 := TouchSet{}
	Extract(&t2, op.Vote{Voter: "bob", Author: "b", Permlink: "p2"})
	// Conservative: any two votes conflict via CommentTouched (the
	// detector lacks per-comment_id granularity at the parse layer).
	if !t1.ConflictsWith(&t2) {
		t.Fatalf("any two votes should conflict (conservative)")
	}
}

func TestTouchSetConflict_WitnessVotesAlwaysConflict(t *testing.T) {
	// Conservative: AccountWitnessVote sets FundsTouched because the
	// HF2 path walks every witness the voter currently votes for via
	// AdjustProxiedWitnessVotes — that transitive write set isn't
	// captured by the (Account, Witness) op fields alone. Two AWVs by
	// different voters + different witnesses still conflict via the
	// FundsTouched flag to keep parallel-apply ordering sound.
	t1 := TouchSet{}
	Extract(&t1, op.AccountWitnessVote{Account: "alice", Witness: "w1"})
	t2 := TouchSet{}
	Extract(&t2, op.AccountWitnessVote{Account: "bob", Witness: "w2"})
	if !t1.ConflictsWith(&t2) {
		t.Fatalf("AccountWitnessVote should be FundsTouched-conservative")
	}
}

func TestTouchSetConflict_SameWitness(t *testing.T) {
	t1 := TouchSet{}
	Extract(&t1, op.AccountWitnessVote{Account: "alice", Witness: "w1"})
	t2 := TouchSet{}
	Extract(&t2, op.AccountWitnessVote{Account: "bob", Witness: "w1"})
	if !t1.ConflictsWith(&t2) {
		t.Fatalf("votes for the same witness should conflict")
	}
}

func TestTouchSetReset(t *testing.T) {
	t1 := TouchSet{}
	Extract(&t1, op.Vote{Voter: "alice"})
	if !t1.CommentTouched {
		t.Fatalf("setup: CommentTouched should be set")
	}
	t1.Reset()
	if t1.CommentTouched || t1.FundsTouched || t1.Accounts.Len() != 0 || t1.Witnesses.Len() != 0 {
		t.Fatalf("Reset did not clear the TouchSet: %+v", t1)
	}
}
