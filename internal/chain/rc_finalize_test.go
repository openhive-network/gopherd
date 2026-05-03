package chain

import (
	"testing"

	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
)

// TestApplyRCDelegationFromTo ports the simplest direct_rc_delegation
// flow: increase, decrease, removal.
func TestApplyRCDelegationFromTo(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}

	s := state.NewSession(db)
	objects.PutAccount(s, &objects.Account{Name: "alice", VestingShares: 1_000_000})
	objects.PutAccount(s, &objects.Account{Name: "bob", VestingShares: 500_000})
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}

	// Initial delegation 100_000.
	s = state.NewSession(db)
	if err := ApplyRCDelegation(s, "alice", "bob", 100_000); err != nil {
		t.Fatal(err)
	}
	a, _ := objects.GetAccount(s, "alice")
	b, _ := objects.GetAccount(s, "bob")
	if a.DelegatedRCOut != 100_000 {
		t.Errorf("alice DelegatedRCOut: got %d, want 100000", a.DelegatedRCOut)
	}
	if b.DelegatedRCIn != 100_000 {
		t.Errorf("bob DelegatedRCIn: got %d, want 100000", b.DelegatedRCIn)
	}
	d, err := objects.GetRCDirectDelegation(s, "alice", "bob")
	if err != nil {
		t.Fatalf("delegation row missing: %v", err)
	}
	if d.DelegatedRC != 100_000 {
		t.Errorf("delegation row: got %d, want 100000", d.DelegatedRC)
	}

	// Increase to 200_000 (delta = +100_000).
	if err := ApplyRCDelegation(s, "alice", "bob", 200_000); err != nil {
		t.Fatal(err)
	}
	a, _ = objects.GetAccount(s, "alice")
	b, _ = objects.GetAccount(s, "bob")
	if a.DelegatedRCOut != 200_000 || b.DelegatedRCIn != 200_000 {
		t.Errorf("post-increase: alice.out=%d bob.in=%d, want 200000/200000",
			a.DelegatedRCOut, b.DelegatedRCIn)
	}

	// Decrease to 50_000 (delta = -150_000).
	if err := ApplyRCDelegation(s, "alice", "bob", 50_000); err != nil {
		t.Fatal(err)
	}
	a, _ = objects.GetAccount(s, "alice")
	b, _ = objects.GetAccount(s, "bob")
	if a.DelegatedRCOut != 50_000 || b.DelegatedRCIn != 50_000 {
		t.Errorf("post-decrease: alice.out=%d bob.in=%d, want 50000/50000",
			a.DelegatedRCOut, b.DelegatedRCIn)
	}

	// Removal (max_rc=0).
	if err := ApplyRCDelegation(s, "alice", "bob", 0); err != nil {
		t.Fatal(err)
	}
	a, _ = objects.GetAccount(s, "alice")
	b, _ = objects.GetAccount(s, "bob")
	if a.DelegatedRCOut != 0 || b.DelegatedRCIn != 0 {
		t.Errorf("post-removal: alice.out=%d bob.in=%d, want 0/0",
			a.DelegatedRCOut, b.DelegatedRCIn)
	}
	if _, err := objects.GetRCDirectDelegation(s, "alice", "bob"); err == nil {
		t.Errorf("delegation row should be deleted after max_rc=0")
	}
}

// TestRCDelegationExpirationRefund ports the consumer side of
// rc_handle_expired_delegations: a queued expiration matures and the
// delegator's DelegatedRCOut is reduced.
func TestRCDelegationExpirationRefund(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}

	s := state.NewSession(db)
	objects.PutAccount(s, &objects.Account{Name: "alice",
		VestingShares: 1_000_000, DelegatedRCOut: 100_000})
	QueueRCDelegationExpiration(s, "alice", 100_000, 1_000_000_000)
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}

	// Run finalize at exactly the expiration time → row matures.
	s = state.NewSession(db)
	rcHandleExpiredDelegations(s, 1_000_000_000+rcDelegationReturnPeriodSec)
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}

	s = state.NewSession(db)
	a, _ := objects.GetAccount(s, "alice")
	if a.DelegatedRCOut != 0 {
		t.Errorf("alice DelegatedRCOut after expiry: got %d, want 0", a.DelegatedRCOut)
	}
}

// TestRCSetPoolParamsHF20Init confirms the rc_pool_object is created
// at the first HF20+ schedule update.
func TestRCSetPoolParamsHF20Init(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}

	s := state.NewSession(db)
	if objects.HasRCPool(s) {
		t.Fatal("RC pool exists pre-HF20")
	}

	// Force-enter HF20 path: call rcSetPoolParams with a block_num past HF20.
	// HF20 activation block is ~26.2M; passing 30M ensures we're past it.
	rcSetPoolParams(s, 30_000_000, 1_500_000_000)
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}

	s = state.NewSession(db)
	if !objects.HasRCPool(s) {
		t.Fatal("RC pool not created after HF20 set_pool_params")
	}
	pool, err := objects.GetRCPool(s)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < objects.RCNumResources; i++ {
		if pool.Pool[i].Update == 0 {
			t.Errorf("pool[%d].Update not initialized", i)
		}
	}
}
