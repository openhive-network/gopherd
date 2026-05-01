package objects

import (
	"reflect"
	"testing"

	"hived-go/internal/state"
)

func openTempDB(t *testing.T) *state.Database {
	t.Helper()
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestAccountRoundTrip(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)

	a := Account{
		Name:          "alice",
		Created:       1_700_000_000,
		Balance:       12345,
		HBDBalance:    -50,
		VestingShares: 9_999_999,
	}
	PutAccount(s, &a)

	got, err := GetAccount(s, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, a) {
		t.Fatalf("round-trip:\n got %+v\nwant %+v", got, a)
	}

	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}

	// Fresh session — value persists.
	s2 := state.NewSession(db)
	got2, err := GetAccount(s2, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got2, a) {
		t.Fatalf("after Commit:\n got %+v\nwant %+v", got2, a)
	}
}

func TestHasAccount(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	if has, err := HasAccount(s, "alice"); err != nil || has {
		t.Fatalf("HasAccount(alice) before Put: has=%v err=%v", has, err)
	}
	PutAccount(s, &Account{Name: "alice"})
	if has, err := HasAccount(s, "alice"); err != nil || !has {
		t.Fatalf("HasAccount(alice) after Put: has=%v err=%v", has, err)
	}
}

func TestDGPORoundTrip(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)

	p := DynamicGlobalProperty{
		HeadBlockNumber:    100_000,
		HeadBlockID:        [20]byte{0x00, 0x01, 0x86, 0xa0, 0xab, 0xcd},
		Time:               1_700_000_000,
		CurrentWitness:     "initminer",
		CurrentSupply:      250_000_000,
		CurrentHBDSupply:   1_000_000,
		TotalVestingShares: 1_000_000_000_000,
	}
	PutDGPO(s, &p)

	got, err := GetDGPO(s)
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Fatalf("round-trip:\n got %+v\nwant %+v", got, p)
	}

	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}
	s2 := state.NewSession(db)
	got2, err := GetDGPO(s2)
	if err != nil {
		t.Fatal(err)
	}
	if got2 != p {
		t.Fatalf("after Commit:\n got %+v\nwant %+v", got2, p)
	}
}

func TestDGPOSingleton(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	if _, err := GetDGPO(s); err != state.ErrNotFound {
		t.Fatalf("GetDGPO before Put: err=%v, want ErrNotFound", err)
	}

	p1 := DynamicGlobalProperty{HeadBlockNumber: 1}
	PutDGPO(s, &p1)
	p2 := DynamicGlobalProperty{HeadBlockNumber: 2}
	PutDGPO(s, &p2)

	got, err := GetDGPO(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadBlockNumber != 2 {
		t.Fatalf("PutDGPO must overwrite: got %d, want 2", got.HeadBlockNumber)
	}
}

func TestAccountAndDGPOTableIsolation(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	PutAccount(s, &Account{Name: "alice", Balance: 100})
	PutDGPO(s, &DynamicGlobalProperty{HeadBlockNumber: 7, CurrentWitness: "init"})
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}

	// Fresh session reads BOTH back.
	s2 := state.NewSession(db)
	a, err := GetAccount(s2, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if a.Balance != 100 {
		t.Fatalf("alice balance = %d, want 100", a.Balance)
	}
	p, err := GetDGPO(s2)
	if err != nil {
		t.Fatal(err)
	}
	if p.HeadBlockNumber != 7 {
		t.Fatalf("dgpo head = %d, want 7", p.HeadBlockNumber)
	}
}
