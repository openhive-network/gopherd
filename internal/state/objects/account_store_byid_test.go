package objects

import (
	"testing"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// checkByIdMirror asserts byId is an exact pointer-for-pointer mirror of
// primary — the invariant every Put/Delete/Restore path must preserve.
func checkByIdMirror(t *testing.T, as *AccountStore, where string) {
	t.Helper()
	if len(as.byId) != len(as.primary) {
		t.Fatalf("%s: byId=%d primary=%d", where, len(as.byId), len(as.primary))
	}
	for name, slot := range as.primary {
		id := auth.InternAccountId(name)
		if as.byId[id] != slot {
			t.Fatalf("%s: byId[%q]=%p primary=%p", where, name, as.byId[id], slot)
		}
	}
}

func TestAccountStoreByIdConsistency(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	as := EnsureAccountStore(s)
	db.UndoEnabled = false

	as.Put(&Account{Name: "alice", Balance: 1})
	as.Put(&Account{Name: "bob", Balance: 2})
	checkByIdMirror(t, as, "after inserts")

	aliceID := auth.InternAccountId("alice")
	if a, ok := as.GetByID(aliceID); !ok || a.Balance != 1 {
		t.Fatalf("GetByID(alice): ok=%v bal=%v", ok, a)
	}
	if a, _ := as.GetByID(aliceID); a != as.primary["alice"] {
		t.Fatal("GetByID must return the same pointer as primary")
	}

	// Overwrite keeps the slot pointer + id; byId tracks the new value.
	as.Put(&Account{Name: "alice", Balance: 100})
	if a, ok := as.GetByID(aliceID); !ok || a.Balance != 100 {
		t.Fatalf("GetByID after overwrite: ok=%v bal=%v", ok, a)
	}
	checkByIdMirror(t, as, "after overwrite")

	as.Delete("bob")
	if _, ok := as.GetByID(auth.InternAccountId("bob")); ok {
		t.Fatal("bob must be gone from byId after Delete")
	}
	checkByIdMirror(t, as, "after delete")

	// Freelist reuse: bob's freed slot may back a new account — byId must
	// point at it under the NEW name's id, not bob's.
	as.Put(&Account{Name: "carol", Balance: 7})
	if a, ok := as.GetByID(auth.InternAccountId("carol")); !ok || a.Balance != 7 {
		t.Fatalf("GetByID(carol) after reuse: ok=%v bal=%v", ok, a)
	}
	checkByIdMirror(t, as, "after freelist reuse")
}

func TestAccountStoreByIdUndo(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	as := EnsureAccountStore(s)
	db.UndoEnabled = true

	as.Put(&Account{Name: "alice", Balance: 1})
	as.CommitPoint() // alice is now the committed baseline

	sp := as.SnapshotPoint()
	as.Put(&Account{Name: "carol", Balance: 9}) // insert inside the savepoint
	as.Delete("alice")                          // delete inside the savepoint
	if _, ok := as.GetByID(auth.InternAccountId("alice")); ok {
		t.Fatal("alice should be deleted pre-restore")
	}
	if _, ok := as.GetByID(auth.InternAccountId("carol")); !ok {
		t.Fatal("carol should exist pre-restore")
	}

	as.RestoreToPoint(sp)
	if _, ok := as.GetByID(auth.InternAccountId("carol")); ok {
		t.Fatal("carol insert must be undone from byId")
	}
	if a, ok := as.GetByID(auth.InternAccountId("alice")); !ok || a.Balance != 1 {
		t.Fatalf("alice delete must be undone in byId: ok=%v bal=%v", ok, a)
	}
	checkByIdMirror(t, as, "after restore")
}
