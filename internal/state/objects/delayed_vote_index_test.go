package objects

import (
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

// TestDelayedVoteIndexOrderedByFront pins the time-ordered candidate index:
// only accounts whose front bucket has matured are returned, the front
// advancing re-keys the account, an emptied queue drops it, and an undo
// that restores an earlier front puts it back where it belongs.
func TestDelayedVoteIndexOrderedByFront(t *testing.T) {
	const interval = uint32(30 * 24 * 3600)
	db := state.OpenMem()
	s := state.NewSession(db)
	st := EnsureAccountStore(s)
	put := func(name auth.AccountName, times ...uint32) {
		a := Account{Name: name}
		for _, tm := range times {
			a.DelayedVotes = append(a.DelayedVotes, DelayedVoteEntry{Time: tm, Val: 1})
		}
		st.Put(&a)
	}
	put("alice", 1000, 5000)
	put("bob", 3000)
	put("carol", 9000)
	put("dave") // no delayed votes: never a candidate

	if !st.DelayedVoteNoneDue(interval+999, interval) {
		t.Fatal("nothing should be due before alice's front matures")
	}
	if st.DelayedVoteNoneDue(interval+1000, interval) {
		t.Fatal("alice is due at exactly front+interval")
	}
	got := st.DelayedVoteCandidatesDue(interval+3000, interval)
	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("due at 3000: %v, want [alice bob]", got)
	}

	// alice's front advances to 5000: she must leave the due set for 3000.
	a, _ := st.Get("alice")
	a.DelayedVotes = a.DelayedVotes[1:]
	st.Put(&a)
	if got := st.DelayedVoteCandidatesDue(interval+3000, interval); len(got) != 1 || got[0] != "bob" {
		t.Fatalf("after alice advanced: %v, want [bob]", got)
	}
	// bob's queue empties: dropped.
	b, _ := st.Get("bob")
	b.DelayedVotes = nil
	st.Put(&b)
	if got := st.DelayedVoteCandidatesDue(interval+5000, interval); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("after bob emptied: %v, want [alice]", got)
	}

	// Undo across alice's advance restores her 1000 front in the index.
	point := st.SnapshotPoint()
	db.UndoEnabled = true
	a, _ = st.Get("alice")
	a.DelayedVotes = a.DelayedVotes[1:] // -> empty
	st.Put(&a)
	if got := st.DelayedVoteCandidatesDue(interval+9000, interval); len(got) != 1 || got[0] != "carol" {
		t.Fatalf("after alice emptied: %v, want [carol]", got)
	}
	st.RestoreToPoint(point)
	if got := st.DelayedVoteCandidatesDue(interval+5000, interval); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("after undo: %v, want [alice] back at front 5000", got)
	}
	if len(st.DelayedVoteCandidates()) != 2 {
		t.Fatalf("candidates = %v, want alice and carol", st.DelayedVoteCandidates())
	}
}
