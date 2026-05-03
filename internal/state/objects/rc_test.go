package objects

import (
	"testing"

	"hived-go/internal/state"
)

// TestRCPoolRoundTrip exercises the rc_pool_object encoder/decoder.
// Mirrors the wire-shape side of the RC tests; full evaluator tests
// live alongside chain.rcFinalizeBlock once HF20 replay coverage lands.
func TestRCPoolRoundTrip(t *testing.T) {
	original := RCPool{}
	for i := 0; i < RCNumResources; i++ {
		original.Pool[i] = RCPoolEntry{
			Current: int64(1000 + i),
			Max:     int64(10000 + i),
			Update:  uint32(1700000000 + i),
			Cost:    int64(100 + i),
		}
	}
	encoded := AppendRCPool(nil, &original)
	decoded, err := ReadRCPool(encoded)
	if err != nil {
		t.Fatalf("ReadRCPool: %v", err)
	}
	for i := 0; i < RCNumResources; i++ {
		if decoded.Pool[i] != original.Pool[i] {
			t.Errorf("pool[%d] mismatch: got %+v, want %+v", i, decoded.Pool[i], original.Pool[i])
		}
	}
}

// TestRCDirectDelegationRoundTrip exercises the wire codec.
func TestRCDirectDelegationRoundTrip(t *testing.T) {
	d := RCDirectDelegation{From: "alice", To: "bob", DelegatedRC: 1234567890}
	encoded := AppendRCDirectDelegation(nil, &d)
	decoded, err := ReadRCDirectDelegation(encoded)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if decoded != d {
		t.Errorf("round-trip: got %+v, want %+v", decoded, d)
	}
}

// TestRCDelegationExpirationRoundTrip exercises the wire codec.
func TestRCDelegationExpirationRoundTrip(t *testing.T) {
	d := RCDelegationExpiration{
		Seq: 42, Delegator: "alice", RC: 9999, Expiration: 1700000000,
	}
	encoded := AppendRCDelegationExpiration(nil, &d)
	decoded, err := ReadRCDelegationExpiration(encoded)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if decoded != d {
		t.Errorf("round-trip: got %+v, want %+v", decoded, d)
	}
}

// TestRCPoolPersistence exercises the table integration: put/get + Has.
func TestRCPoolPersistence(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := state.NewSession(db)

	if HasRCPool(s) {
		t.Fatal("HasRCPool should be false before any Put")
	}

	want := RCPool{}
	for i := 0; i < RCNumResources; i++ {
		want.Pool[i] = RCPoolEntry{Max: int64(1 << uint(i)), Cost: 1, Update: 1}
	}
	PutRCPool(s, &want)
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}

	s2 := state.NewSession(db)
	if !HasRCPool(s2) {
		t.Fatal("HasRCPool false after Put")
	}
	got, err := GetRCPool(s2)
	if err != nil {
		t.Fatalf("GetRCPool: %v", err)
	}
	for i := 0; i < RCNumResources; i++ {
		if got.Pool[i] != want.Pool[i] {
			t.Errorf("pool[%d]: got %+v, want %+v", i, got.Pool[i], want.Pool[i])
		}
	}
}

// TestAccountMaximumRC ports the formula side of rc_tests.cpp's
// max_rc derivation: max_rc = effective_vesting + delegated_in - delegated_out
// (HF22+).
func TestAccountMaximumRC(t *testing.T) {
	a := Account{
		VestingShares:          10_000,
		DelegatedVestingShares: 1_000,
		ReceivedVestingShares:  500,
		DelegatedRCIn:          100,
		DelegatedRCOut:         50,
	}
	// effective_vesting = 10000 - 1000 + 500 = 9500
	// max_rc = 9500 + 100 - 50 = 9550
	if got := a.MaximumRC(); got != 9550 {
		t.Errorf("MaximumRC: got %d, want 9550", got)
	}

	// Negative-vesting case: clamp to zero.
	a2 := Account{
		VestingShares:          100,
		DelegatedVestingShares: 1_000_000, // far more delegated out than held
	}
	if got := a2.MaximumRC(); got != 0 {
		t.Errorf("MaximumRC clamp: got %d, want 0", got)
	}
}

// TestRegenerateRCMana exercises the manabar regeneration formula.
func TestRegenerateRCMana(t *testing.T) {
	a := Account{
		VestingShares:    1_000_000_000, // big enough max_rc that we don't hit the cap
		RCManaCurrent:    0,
		RCManaLastUpdate: 1_000_000_000,
		LastMaxRC:        1_000_000_000,
	}
	// Advance by exactly 1/2 of the regen window (2.5 days).
	now := uint32(1_000_000_000) + RCRegenTimeSec/2
	RegenerateRCMana(&a, now)
	if a.RCManaLastUpdate != now {
		t.Errorf("LastUpdate: got %d, want %d", a.RCManaLastUpdate, now)
	}
	// At t=now, regen ≈ max_rc / 2.
	wantMin := int64(490_000_000)
	wantMax := int64(510_000_000)
	if a.RCManaCurrent < wantMin || a.RCManaCurrent > wantMax {
		t.Errorf("RCManaCurrent: got %d, want roughly %d±2%%", a.RCManaCurrent, (wantMin+wantMax)/2)
	}

	// Full window from current => cap to max.
	RegenerateRCMana(&a, now+RCRegenTimeSec)
	if a.RCManaCurrent != a.LastMaxRC {
		t.Errorf("RCManaCurrent at full regen: got %d, want %d", a.RCManaCurrent, a.LastMaxRC)
	}
}

// TestUseManaSaturatesAtMin exercises the saturating subtractor used
// for RC charging.
func TestUseManaSaturatesAtMin(t *testing.T) {
	// Normal subtraction.
	if got := UseMana(1000, 300, 0); got != 700 {
		t.Errorf("UseMana(1000, 300, 0): got %d, want 700", got)
	}
	// Subtraction saturates at the floor (replay never enforces a
	// positive floor — passes -max as floor).
	if got := UseMana(100, 500, 0); got != 0 {
		t.Errorf("UseMana(100, 500, 0): got %d, want 0", got)
	}
	// Floor passed below zero allows negative.
	if got := UseMana(100, 500, -1000); got != -400 {
		t.Errorf("UseMana(100, 500, -1000): got %d, want -400", got)
	}
}
