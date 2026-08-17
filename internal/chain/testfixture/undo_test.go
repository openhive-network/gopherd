package testfixture

import (
	"reflect"
	"testing"

	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// Port of undo_tests.cpp::undo_basic.
//
// Operates directly on the state.Session overlay (the Go equivalent
// of chainbase's undo_session). Each scenario:
//
//  1. snapshot the committed state
//  2. open a session, do mutations
//  3. drop the overlay (Session.Undo)
//  4. verify the committed state is unchanged
//
// This is the unit-test counterpart of an apply_block being rolled
// back due to evaluator failure or fork divergence.
func TestUndoBasicAccountCreate(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	// Pre-state snapshot.
	preExists, _ := objects.HasAccount(state.NewSession(f.State), "name00")
	if preExists {
		t.Fatal("name00 should not exist before scenario")
	}

	// Scenario 1: create then undo.
	s := state.NewSession(f.State)
	objects.PutAccount(s, &objects.Account{Name: "name00"})
	s.Undo()

	// After undo, name00 must not be visible in a fresh session.
	s2 := state.NewSession(f.State)
	if has, _ := objects.HasAccount(s2, "name00"); has {
		t.Fatal("name00 should not exist after Undo")
	}
}

func TestUndoBasicCreateModifyUndo(t *testing.T) {
	f := New(t)
	f.GenerateBlock()
	s := state.NewSession(f.State)

	// Create + modify + undo.
	objects.PutAccount(s, &objects.Account{Name: "name00", Balance: 100})
	got, _ := objects.GetAccount(s, "name00")
	if got.Balance != 100 {
		t.Fatalf("pre-modify Balance: got %d, want 100", got.Balance)
	}
	got.Balance = 200
	objects.PutAccount(s, &got)
	got2, _ := objects.GetAccount(s, "name00")
	if got2.Balance != 200 {
		t.Fatalf("post-modify Balance: got %d, want 200", got2.Balance)
	}
	s.Undo()

	// After undo, the account must not exist.
	s2 := state.NewSession(f.State)
	if has, _ := objects.HasAccount(s2, "name00"); has {
		t.Fatal("name00 should not exist after Undo")
	}
}

// Port of undo_tests.cpp::undo_basic --- "1 object (create, modify, remove)".
func TestUndoBasicCreateModifyRemoveUndo(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	// Pre-state: nothing.
	s := state.NewSession(f.State)
	objects.PutAccount(s, &objects.Account{Name: "name00", Balance: 100})
	a, _ := objects.GetAccount(s, "name00")
	a.Balance = 200
	objects.PutAccount(s, &a)
	s.Delete(objects.AccountTable, state.PrimaryIndex, objects.AccountKey("name00"))
	s.Undo()

	// Verify state is clean.
	s2 := state.NewSession(f.State)
	if has, _ := objects.HasAccount(s2, "name00"); has {
		t.Fatal("name00 should not exist after Undo")
	}
}

// Port of undo_tests.cpp::undo_basic --- multi-object scenario:
// create A, create B + modify, create C + remove. Then undo.
func TestUndoBasicMultiObject(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	pre := state.NewSession(f.State)
	preCount := 0
	_ = objects.RangeAccounts(pre, func(_ *objects.Account) (bool, error) {
		preCount++
		return true, nil
	})

	s := state.NewSession(f.State)
	objects.PutAccount(s, &objects.Account{Name: "name00", Balance: 1})
	objects.PutAccount(s, &objects.Account{Name: "name01", Balance: 2})
	a, _ := objects.GetAccount(s, "name01")
	a.Balance = 22
	objects.PutAccount(s, &a)
	objects.PutAccount(s, &objects.Account{Name: "name03", Balance: 3})
	objects.DeleteAccount(s, "name03")
	s.Undo()

	// Verify final state matches pre-state.
	post := state.NewSession(f.State)
	postCount := 0
	_ = objects.RangeAccounts(post, func(_ *objects.Account) (bool, error) {
		postCount++
		return true, nil
	})
	if postCount != preCount {
		t.Fatalf("account count after undo: got %d, want %d", postCount, preCount)
	}
}

// Round-trip: commit then re-read. Confirms the session.Commit /
// fresh-session split produces visible writes (the pre-condition for
// every test above to be meaningful).
func TestUndoCommitVisibility(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	a := objects.Account{Name: "alice", Balance: 999}
	s := state.NewSession(f.State)
	objects.PutAccount(s, &a)
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}

	s2 := state.NewSession(f.State)
	got, err := objects.GetAccount(s2, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, a) {
		t.Fatalf("round-trip:\n got %+v\nwant %+v", got, a)
	}
}
