package testfixture

import (
	"strings"
	"testing"

	"hived-go/internal/protocol/op"
)

// Port of operation_tests.cpp::transfer_apply.
//
// Hived test outline:
//   - alice gets 10 HIVE; bob gets 1 HBD via issue_funds
//   - alice transfers 5 HIVE → bob; balances become alice=5, bob=5
//   - alice transfers another 5 HIVE → bob; alice=0, bob=10
//   - third transfer fails with "insufficient balance"
//   - HF24+: bob transfers 1 HIVE → treasury; converts to 1 HBD on treasury
//   - HF24+: bob transfers 1 HBD → treasury; arrives as HBD
//
// We omit the treasury HF24 path until our HF24 fixture support
// matches; the core balance-mutation cases all run.
func TestTransferApply(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()

	f.FundLiquid("alice", 10_000)  // 10.000 HIVE
	f.FundLiquidHBD("bob", 1_000)  // 1.000 HBD

	if got := f.Balance("alice"); got != 10_000 {
		t.Fatalf("initial alice.HIVE: got %d, want 10000", got)
	}
	if got := f.Balance("bob"); got != 0 {
		t.Fatalf("initial bob.HIVE: got %d, want 0", got)
	}

	t.Log("--- Test normal transaction: alice → bob 5.000 TESTS")
	if err := f.ApplyOp(op.Transfer{
		From:   "alice",
		To:     "bob",
		Amount: HIVE(5_000),
	}); err != nil {
		t.Fatalf("transfer 1: %v", err)
	}
	if got := f.Balance("alice"); got != 5_000 {
		t.Fatalf("alice.HIVE after first transfer: got %d, want 5000", got)
	}
	if got := f.Balance("bob"); got != 5_000 {
		t.Fatalf("bob.HIVE after first transfer: got %d, want 5000", got)
	}

	t.Log("--- Test emptying an account: alice → bob 5.000 more")
	if err := f.ApplyOp(op.Transfer{
		From:   "alice",
		To:     "bob",
		Amount: HIVE(5_000),
	}); err != nil {
		t.Fatalf("transfer 2: %v", err)
	}
	if got := f.Balance("alice"); got != 0 {
		t.Fatalf("alice.HIVE after empty: got %d, want 0", got)
	}
	if got := f.Balance("bob"); got != 10_000 {
		t.Fatalf("bob.HIVE after empty: got %d, want 10000", got)
	}

	t.Log("--- Test transferring non-existent funds (alice has 0)")
	err := f.ApplyOp(op.Transfer{
		From:   "alice",
		To:     "bob",
		Amount: HIVE(5_000),
	})
	if err == nil {
		t.Fatal("expected insufficient-balance failure, got nil")
	}
	if !strings.Contains(err.Error(), "insufficient balance") {
		t.Fatalf("expected insufficient-balance error, got: %v", err)
	}
	if got := f.Balance("alice"); got != 0 {
		t.Fatalf("alice.HIVE after failed transfer: got %d, want 0", got)
	}
	if got := f.Balance("bob"); got != 10_000 {
		t.Fatalf("bob.HIVE after failed transfer: got %d, want 10000", got)
	}
}

// Port of operation_tests.cpp::transfer_to_vesting_apply (subset).
//
// alice has 10 HIVE; she transfers 7.5 HIVE to her own vesting,
// receiving the genesis 1:1000 ratio of new VESTS.
func TestTransferToVestingApply(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 10_000)
	f.GenerateBlock()

	t.Log("--- Test self transfer to vesting (alice → alice 7.500 HIVE)")
	preVests := f.Vests("alice")
	if err := f.ApplyOp(op.TransferToVesting{
		From:   "alice",
		To:     "alice",
		Amount: HIVE(7_500),
	}); err != nil {
		t.Fatalf("self vest: %v", err)
	}
	if got := f.Balance("alice"); got != 2_500 {
		t.Fatalf("alice.HIVE after self-vest: got %d, want 2500", got)
	}
	gainedVests := f.Vests("alice") - preVests
	// At genesis 1:1000 ratio (or close to it): 7500 HIVE-sat → 7,500,000 VESTS-sat.
	if gainedVests <= 0 {
		t.Fatalf("alice should have gained VESTS, got delta=%d", gainedVests)
	}

	t.Log("--- Test transfer to vesting alice → bob 2.000 HIVE")
	preBobVests := f.Vests("bob")
	if err := f.ApplyOp(op.TransferToVesting{
		From:   "alice",
		To:     "bob",
		Amount: HIVE(2_000),
	}); err != nil {
		t.Fatalf("vest to bob: %v", err)
	}
	if got := f.Balance("alice"); got != 500 {
		t.Fatalf("alice.HIVE after vest-to-bob: got %d, want 500", got)
	}
	if got := f.Vests("bob") - preBobVests; got <= 0 {
		t.Fatalf("bob should have gained VESTS, got delta=%d", got)
	}

	t.Log("--- Test failure when sender lacks balance")
	err := f.ApplyOp(op.TransferToVesting{
		From:   "alice",
		To:     "bob",
		Amount: HIVE(10_000),
	})
	if err == nil {
		t.Fatal("expected insufficient-balance, got nil")
	}
	if !strings.Contains(err.Error(), "insufficient balance") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Port of operation_tests.cpp::withdraw_vesting_apply (subset).
//
// alice has 10000 VESTS; she powers down all of them. Pre-HF16 the
// withdraw_rate is amount/104; first chunk fires after 1 week.
func TestWithdrawVestingApply(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.FundVesting("alice", 10_000) // 10 HIVE → 10,000,000 VESTS-sat
	f.GenerateBlock()

	preVests := f.Vests("alice")
	if preVests <= 0 {
		t.Fatalf("alice should have VESTS, got %d", preVests)
	}

	t.Log("--- Test setting up withdraw schedule")
	if err := f.ApplyOp(op.WithdrawVesting{
		Account:       "alice",
		VestingShares: VESTS(preVests),
	}); err != nil {
		t.Fatalf("withdraw_vesting: %v", err)
	}
	a := f.Account_("alice")
	if a.VestingWithdrawRate <= 0 {
		t.Fatal("withdraw rate should be > 0")
	}
	if a.ToWithdraw != preVests {
		t.Fatalf("ToWithdraw: got %d, want %d", a.ToWithdraw, preVests)
	}
	if a.NextVestingWithdrawal == 0 {
		t.Fatal("NextVestingWithdrawal should be set")
	}
}
