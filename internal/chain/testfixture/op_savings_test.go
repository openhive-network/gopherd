package testfixture

import (
	"testing"

	"hived-go/internal/protocol/op"
)

// Port of operation_tests.cpp::transfer_to_savings_apply.
//
// alice has 10 HIVE + 10 HBD; tests:
//   - failure: insufficient HIVE
//   - failure: recipient doesn't exist
//   - success: transfer to self HIVE / HBD
//   - success: transfer to other (bob)
func TestTransferToSavingsApply(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()
	f.FundLiquid("alice", 10_000)
	f.FundLiquidHBD("alice", 10_000)

	if got := f.Balance("alice"); got != 10_000 {
		t.Fatalf("alice HIVE: got %d, want 10000", got)
	}
	if got := f.HBDBalance("alice"); got != 10_000 {
		t.Fatalf("alice HBD: got %d, want 10000", got)
	}

	t.Log("--- Failure with insufficient HIVE")
	if err := f.ApplyOp(op.TransferToSavings{
		From:   "alice",
		To:     "alice",
		Amount: HIVE(20_000),
	}); err == nil {
		t.Fatal("expected insufficient-balance failure")
	}

	t.Log("--- Failure when transferring to non-existent account")
	if err := f.ApplyOp(op.TransferToSavings{
		From:   "alice",
		To:     "nobody",
		Amount: HIVE(1_000),
	}); err == nil {
		t.Fatal("expected account-not-found failure")
	}

	t.Log("--- Success transferring HIVE to self")
	if err := f.ApplyOp(op.TransferToSavings{
		From:   "alice",
		To:     "alice",
		Amount: HIVE(1_000),
	}); err != nil {
		t.Fatalf("self-savings HIVE: %v", err)
	}
	if got := f.Balance("alice"); got != 9_000 {
		t.Fatalf("alice HIVE after self-savings: got %d, want 9000", got)
	}
	if got := f.SavingsBalance("alice"); got != 1_000 {
		t.Fatalf("alice savings HIVE: got %d, want 1000", got)
	}

	t.Log("--- Success transferring HBD to self")
	if err := f.ApplyOp(op.TransferToSavings{
		From:   "alice",
		To:     "alice",
		Amount: HBD(1_000),
	}); err != nil {
		t.Fatalf("self-savings HBD: %v", err)
	}
	if got := f.HBDBalance("alice"); got != 9_000 {
		t.Fatalf("alice HBD after self-savings: got %d, want 9000", got)
	}
	if got := f.SavingsHBDBalance("alice"); got != 1_000 {
		t.Fatalf("alice savings HBD: got %d, want 1000", got)
	}

	t.Log("--- Success transferring HIVE to other (bob)")
	if err := f.ApplyOp(op.TransferToSavings{
		From:   "alice",
		To:     "bob",
		Amount: HIVE(1_000),
	}); err != nil {
		t.Fatalf("alice→bob savings HIVE: %v", err)
	}
	if got := f.Balance("alice"); got != 8_000 {
		t.Fatalf("alice HIVE: got %d, want 8000", got)
	}
	if got := f.SavingsBalance("bob"); got != 1_000 {
		t.Fatalf("bob savings HIVE: got %d, want 1000", got)
	}

	t.Log("--- Success transferring HBD to other (bob)")
	if err := f.ApplyOp(op.TransferToSavings{
		From:   "alice",
		To:     "bob",
		Amount: HBD(1_000),
	}); err != nil {
		t.Fatalf("alice→bob savings HBD: %v", err)
	}
	if got := f.HBDBalance("alice"); got != 8_000 {
		t.Fatalf("alice HBD: got %d, want 8000", got)
	}
	if got := f.SavingsHBDBalance("bob"); got != 1_000 {
		t.Fatalf("bob savings HBD: got %d, want 1000", got)
	}
}

// Port of operation_tests.cpp::transfer_from_savings_apply (subset).
//
// Tests the schedule entry creation and the cancel flow. The actual
// 3-day matured payout is exercised in TestSavingsWithdrawMatures.
func TestTransferFromSavingsApply(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.FundLiquid("alice", 10_000)
	f.GenerateBlock()
	// Move 5 HIVE to savings.
	if err := f.ApplyOp(op.TransferToSavings{
		From: "alice", To: "alice", Amount: HIVE(5_000),
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if got := f.SavingsBalance("alice"); got != 5_000 {
		t.Fatalf("alice savings: got %d, want 5000", got)
	}

	t.Log("--- Schedule a withdrawal")
	if err := f.ApplyOp(op.TransferFromSavings{
		From:      "alice",
		RequestID: 1,
		To:        "alice",
		Amount:    HIVE(2_000),
	}); err != nil {
		t.Fatalf("transfer_from_savings: %v", err)
	}
	if got := f.SavingsBalance("alice"); got != 3_000 {
		t.Fatalf("alice savings after schedule: got %d, want 3000", got)
	}
	// Liquid balance unchanged until the 3-day window matures.
	if got := f.Balance("alice"); got != 5_000 {
		t.Fatalf("alice HIVE: got %d, want 5000", got)
	}

	t.Log("--- Cancel the withdrawal")
	if err := f.ApplyOp(op.CancelTransferFromSavings{
		From:      "alice",
		RequestID: 1,
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := f.SavingsBalance("alice"); got != 5_000 {
		t.Fatalf("alice savings after cancel: got %d, want 5000", got)
	}
}

// TestSavingsWithdrawMatures exercises the per-block hook that pays
// out matured savings withdrawals (3-day window).
func TestSavingsWithdrawMatures(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.FundLiquid("alice", 10_000)
	f.GenerateBlock()
	if err := f.ApplyOp(op.TransferToSavings{
		From: "alice", To: "alice", Amount: HIVE(5_000),
	}); err != nil {
		t.Fatalf("setup1: %v", err)
	}
	if err := f.ApplyOp(op.TransferFromSavings{
		From: "alice", RequestID: 1, To: "alice", Amount: HIVE(2_000),
	}); err != nil {
		t.Fatalf("setup2: %v", err)
	}

	preLiquid := f.Balance("alice")
	preSavings := f.SavingsBalance("alice")

	// Skip 3 days + a bit.
	f.SkipSeconds(3*24*60*60 + 60)

	if got := f.Balance("alice") - preLiquid; got != 2_000 {
		t.Fatalf("alice liquid delta after 3d: got %d, want 2000", got)
	}
	if got := f.SavingsBalance("alice"); got != preSavings {
		t.Fatalf("alice savings should be unchanged: got %d, want %d", got, preSavings)
	}
}
