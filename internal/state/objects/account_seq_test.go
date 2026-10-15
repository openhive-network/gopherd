package objects

import (
	"testing"

	"gopherd/internal/state"
)

// TestAccountSeqAssignedOnCreate pins hived's account id order: an account
// gets the next Seq on its first write and keeps it on every later write,
// and the codec carries it.
func TestAccountSeqAssignedOnCreate(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := state.NewSession(db)
	SetAccountSeqCounter(s, 5)
	PutAccount(s, &Account{Name: "zed", Created: 10})
	PutAccount(s, &Account{Name: "abe", Created: 10})
	a, _ := GetAccount(s, "zed")
	b, _ := GetAccount(s, "abe")
	if a.Seq != 6 || b.Seq != 7 {
		t.Fatalf("seq zed=%d abe=%d, want 6, 7", a.Seq, b.Seq)
	}
	a.Balance = 1
	PutAccount(s, &a)
	if again, _ := GetAccount(s, "zed"); again.Seq != 6 || AccountSeqCounter(s) != 7 {
		t.Fatalf("rewrite changed seq to %d (counter %d)", again.Seq, AccountSeqCounter(s))
	}
	// Same block, so only Seq tells them apart: zed was created first.
	if !AccountCreatedBefore(a.Seq, a.Created, a.Name, b.Seq, b.Created, b.Name) {
		t.Fatal("ordering ignores Seq")
	}
	// Without Seq (older state) it falls back to name.
	if AccountCreatedBefore(0, 10, "zed", 0, 10, "abe") {
		t.Fatal("fallback ordering is not by name")
	}
	rt, err := ReadAccount(AppendAccount(nil, &a))
	if err != nil || rt.Seq != 6 {
		t.Fatalf("codec round trip: seq=%d err=%v", rt.Seq, err)
	}
}
