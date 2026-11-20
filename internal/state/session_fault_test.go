package state

import (
	"errors"
	"testing"
)

// TestSessionFaultUnwindsWithSavepoint pins the sticky fault's scoping: the
// first Fail wins, a Restore to a savepoint taken before the fault clears it
// (a rolled-back transaction leaves nothing behind, as hived's undo session
// unwinds the exception), a fault from before the savepoint survives, and
// the block boundary resets it.
func TestSessionFaultUnwindsWithSavepoint(t *testing.T) {
	db, err := Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewSession(db)
	defer s.Close()

	first, second := errors.New("first"), errors.New("second")
	sp := s.Snapshot()
	s.Fail(nil)
	if s.Fault() != nil {
		t.Fatal("Fail(nil) recorded a fault")
	}
	s.Fail(first)
	s.Fail(second)
	if s.Fault() != first {
		t.Fatalf("fault %v, want the first one", s.Fault())
	}
	s.Restore(sp)
	if s.Fault() != nil {
		t.Fatalf("fault %v survived the restore of the transaction that raised it", s.Fault())
	}

	s.Fail(first)
	sp = s.Snapshot()
	s.Fail(second)
	s.Restore(sp)
	if s.Fault() != first {
		t.Fatalf("fault %v after restore, want the one from before the savepoint", s.Fault())
	}
	s.Undo()
	if s.Fault() != nil {
		t.Fatalf("fault %v survived Undo", s.Fault())
	}
	s.Fail(first)
	s.SoftCommit()
	if s.Fault() != nil {
		t.Fatalf("fault %v survived SoftCommit", s.Fault())
	}
}
