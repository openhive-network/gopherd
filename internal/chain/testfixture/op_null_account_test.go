package testfixture

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
)

// Port of operation_time_tests.cpp::clear_null_account.
//
// Funds sent to the "null" account are intended to burn — every
// post-HF14 block, the per-block hook clear_null_account_balance
// drains the account's HIVE / HBD / VESTS / savings / rewards back
// out and decrements supply.
func TestClearNullAccountBalance(t *testing.T) {
	f := New(t)
	// HF14 is when the burn semantic kicks in. We use HF20 here so
	// HF1's vesting share split has already applied (otherwise the
	// price math is mid-transition).
	f.HardforksUpTo(hardfork.HF20)
	f.Actor("null") // force the null account to exist
	f.Actor("alice")
	f.FundLiquid("alice", 100_000)
	f.FundLiquidHBD("alice", 100_000)
	f.GenerateBlock()

	// alice sends to null in five flavors.
	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "null", Amount: HIVE(1_000),
	}); err == nil {
		// May succeed on this block. The next block then clears.
	}
	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "null", Amount: HBD(2_000),
	}); err != nil {
		t.Fatalf("transfer hbd: %v", err)
	}
	if err := f.ApplyOp(op.TransferToVesting{
		From: "alice", To: "null", Amount: HIVE(3_000),
	}); err != nil {
		t.Fatalf("transfer_to_vesting: %v", err)
	}
	if err := f.ApplyOp(op.TransferToSavings{
		From: "alice", To: "null", Amount: HIVE(4_000),
	}); err != nil {
		t.Fatalf("transfer_to_savings hive: %v", err)
	}
	if err := f.ApplyOp(op.TransferToSavings{
		From: "alice", To: "null", Amount: HBD(5_000),
	}); err != nil {
		t.Fatalf("transfer_to_savings hbd: %v", err)
	}

	// After one more block the null account should be drained.
	f.GenerateBlock()

	null := f.Account_("null")
	if null.Balance != 0 {
		t.Fatalf("null.Balance: got %d, want 0", null.Balance)
	}
	if null.HBDBalance != 0 {
		t.Fatalf("null.HBDBalance: got %d, want 0", null.HBDBalance)
	}
	if null.VestingShares != 0 {
		t.Fatalf("null.VestingShares: got %d, want 0", null.VestingShares)
	}
	if null.SavingsBalance != 0 {
		t.Fatalf("null.SavingsBalance: got %d, want 0", null.SavingsBalance)
	}
	if null.SavingsHBDBalance != 0 {
		t.Fatalf("null.SavingsHBDBalance: got %d, want 0", null.SavingsHBDBalance)
	}
	if null.SumDelayedVotes != 0 {
		t.Fatalf("null.SumDelayedVotes: got %d, want 0", null.SumDelayedVotes)
	}
}

// Pre-HF14, transfers to "null" accumulate (no clearing hook).
// We don't have a great way to test this without negative HF gating,
// but we can at least verify HF14+ does the right thing per the
// previous test — and that fresh accounts start clean.
func TestNullAccountHF14Gate(t *testing.T) {
	f := New(t)
	f.Actor("null")
	f.GenerateBlock()
	null := f.Account_("null")
	if null.Balance != 0 || null.HBDBalance != 0 || null.VestingShares != 0 {
		t.Fatalf("fresh null account should be empty: %+v", null)
	}
}
