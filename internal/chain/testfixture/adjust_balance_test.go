package testfixture

import (
	"testing"

	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// TestAdjustBalance ports basic_tests.cpp::adjust_balance_test. Hived's
// adjust_balance(account, asset) credits or debits an account's HIVE
// or HBD liquid balance with an FC_ASSERT on insufficient funds.
//
// We don't expose adjust_balance as a public API in Go (balances are
// mutated through evaluators). The equivalent observable behavior is:
//
//   - Funding via FundLiquid / FundLiquidHBD increases the balance.
//   - A transfer that would drive the sender below zero is rejected.
//   - A successful transfer net-debits the source.
//
// The test exercises both add (HIVE and HBD) and deduct (via transfer
// op) paths to mirror the C++ scenarios.
func TestAdjustBalance(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()

	// --- Add HIVE.
	f.FundLiquid("alice", 50_000)
	if got := f.Balance("alice"); got != 50_000 {
		t.Fatalf("alice HIVE after credit: got %d, want 50000", got)
	}

	// --- Add HBD.
	f.FundLiquidHBD("alice", 100_000)
	if got := f.HBDBalance("alice"); got != 100_000 {
		t.Fatalf("alice HBD after credit: got %d, want 100000", got)
	}

	// --- Deduct HIVE: transfer alice→bob 30_000.
	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "bob", Amount: HIVE(30_000),
	}); err != nil {
		t.Fatalf("transfer 30000 HIVE: %v", err)
	}
	if got := f.Balance("alice"); got != 20_000 {
		t.Fatalf("alice HIVE after 30000 debit: got %d, want 20000", got)
	}
	if got := f.Balance("bob"); got != 30_000 {
		t.Fatalf("bob HIVE after 30000 credit: got %d, want 30000", got)
	}

	// --- Drain remaining HIVE.
	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "bob", Amount: HIVE(20_000),
	}); err != nil {
		t.Fatalf("transfer 20000 HIVE: %v", err)
	}
	if got := f.Balance("alice"); got != 0 {
		t.Fatalf("alice HIVE after full drain: got %d, want 0", got)
	}

	// --- Insufficient HIVE: transfer of 1 should fail.
	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "bob", Amount: HIVE(1),
	}); err == nil {
		t.Fatalf("expected insufficient-balance error")
	}

	// --- Deduct HBD: drain in three transfers.
	for _, amount := range []int64{50_000, 25_000, 25_000} {
		if err := f.ApplyOp(op.Transfer{
			From: "alice", To: "bob", Amount: HBD(amount),
		}); err != nil {
			t.Fatalf("transfer %d HBD: %v", amount, err)
		}
	}
	if got := f.HBDBalance("alice"); got != 0 {
		t.Fatalf("alice HBD after full drain: got %d, want 0", got)
	}

	// --- Insufficient HBD: transfer of 1 should fail.
	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "bob", Amount: HBD(1),
	}); err == nil {
		t.Fatalf("expected insufficient HBD-balance error")
	}

	// Sanity: read alice's record one final time to ensure no spurious
	// negative balance accumulated.
	a, err := objects.GetAccount(state.NewSession(f.State), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if a.Balance != 0 || a.HBDBalance != 0 {
		t.Fatalf("alice ends with HIVE=%d HBD=%d, want 0/0", a.Balance, a.HBDBalance)
	}
}
