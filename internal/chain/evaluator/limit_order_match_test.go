package evaluator

import (
	"testing"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
)

// TestLimitOrderMatching creates two crossing orders and verifies that
// matching works end-to-end:
//
//	alice: sell 100 HIVE for 50 HBD     (price = 2 HIVE / HBD)
//	bob:   sell 50 HBD for 100 HIVE     (price = 0.5 HBD / HIVE)
//
// They should fully fill against each other.
func TestLimitOrderMatching(t *testing.T) {
	dir := t.TempDir()
	db, err := state.Open(dir, 16<<20)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatalf("genesis: %v", err)
	}
	s := state.NewSession(db)

	// Seed alice (has 100 HIVE) and bob (has 50 HBD).
	objects.PutAccount(s, &objects.Account{
		Name:                 "alice",
		Created:              1000,
		Balance:              100_000, // 100.000 HIVE
		VotingManaCurrent:    10000,
		VotingManaLastUpdate: 1000,
	})
	objects.PutAccount(s, &objects.Account{
		Name:                 "bob",
		Created:              1000,
		HBDBalance:           50_000, // 50.000 HBD
		VotingManaCurrent:    10000,
		VotingManaLastUpdate: 1000,
	})
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Block 2,000,000 — well past HF6.
	ctx := &Context{
		DB:        state.NewSession(db),
		Hardforks: hardfork.SetForBlock(2_000_000),
		BlockNum:  2_000_000,
		BlockTime: 1_000_000,
	}
	// Alice creates order: sell 100 HIVE for 50 HBD.
	if err := applyLimitOrderCreate(ctx, "alice", 1,
		asset.Asset{Amount: 100_000, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
		asset.Asset{Amount: 50_000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		2_000_000+86400, false); err != nil {
		t.Fatalf("alice order: %v", err)
	}
	// Verify alice's order is in the by_price index.
	count := 0
	_ = ctx.DB.Range(objects.LimitOrderTable, objects.LimitOrderByPriceIndex, nil,
		func(body, _ []byte) (bool, error) {
			count++
			t.Logf("by_price entry: % x (len=%d)", body, len(body))
			return true, nil
		})
	t.Logf("by_price entries after alice: %d", count)
	t.Logf("alice symbols: HIVE=%#x HBD=%#x", uint32(asset.HiveSymbol), uint32(asset.HbdSymbol))

	// Bob creates order: sell 50 HBD for 100 HIVE — this should match Alice.
	if err := applyLimitOrderCreate(ctx, "bob", 1,
		asset.Asset{Amount: 50_000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		asset.Asset{Amount: 100_000, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
		2_000_000+86400, false); err != nil {
		t.Fatalf("bob order: %v", err)
	}

	if err := ctx.DB.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Verify final balances. Both orders fully fill:
	//   alice: -100 HIVE, +50 HBD
	//   bob:   -50 HBD,  +100 HIVE
	check := state.NewSession(db)
	alice, err := objects.GetAccount(check, "alice")
	if err != nil {
		t.Fatalf("read alice: %v", err)
	}
	if alice.Balance != 0 {
		t.Errorf("alice.Balance: got %d, want 0", alice.Balance)
	}
	if alice.HBDBalance != 50_000 {
		t.Errorf("alice.HBDBalance: got %d, want 50000", alice.HBDBalance)
	}
	bob, err := objects.GetAccount(check, "bob")
	if err != nil {
		t.Fatalf("read bob: %v", err)
	}
	if bob.HBDBalance != 0 {
		t.Errorf("bob.HBDBalance: got %d, want 0", bob.HBDBalance)
	}
	if bob.Balance != 100_000 {
		t.Errorf("bob.Balance: got %d, want 100000", bob.Balance)
	}
	// Both orders should be removed.
	if has, _ := objects.HasLimitOrder(check, "alice", 1); has {
		t.Error("alice's order should have been fully filled and removed")
	}
	if has, _ := objects.HasLimitOrder(check, "bob", 1); has {
		t.Error("bob's order should have been fully filled and removed")
	}
}
