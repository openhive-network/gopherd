package testfixture

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// TestRecurrentTransferExpirationFullPort ports
// operation_time_tests.cpp::recurrent_transfer_expiration in full,
// asserting the row deletion AFTER the final execution + cumulative
// recipient balance. The smaller TestRecurrentTransferExpiration in
// basic_test.go covers the basic schedule wiring.
func TestRecurrentTransferExpirationFullPort(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF25)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 100_000)
	f.GenerateBlock()

	// 2 executions every 24 hours.
	if err := f.ApplyOp(op.RecurrentTransfer{
		From: "alice", To: "bob", Amount: HIVE(5_000),
		Recurrence: 24, Executions: 2, Memo: "x",
	}); err != nil {
		t.Fatalf("recurrent_transfer: %v", err)
	}

	// Right after creation, one row exists for alice→bob.
	s := state.NewSession(f.State)
	if _, err := objects.GetRecurrentTransfer(s, "alice", "bob", 0); err != nil {
		t.Fatalf("recurrent_transfer not stored: %v", err)
	}

	// First firing is at the immediate next block (Trigger = creation time).
	bobBefore := f.Balance("bob")
	f.SkipSeconds(24 * 3600) // one full recurrence
	bobAfter := f.Balance("bob")
	if bobAfter <= bobBefore {
		t.Errorf("bob balance after first fire: got %d, want > %d", bobAfter, bobBefore)
	}

	// Advance past the second firing — should drain the schedule.
	f.SkipSeconds(24*3600 + 60)
	s = state.NewSession(f.State)
	if _, err := objects.GetRecurrentTransfer(s, "alice", "bob", 0); err == nil {
		t.Errorf("recurrent_transfer still present after all executions consumed")
	}

	// Bob should have received 2 × 5_000 = 10_000 total.
	if got := f.Balance("bob"); got < 10_000 {
		t.Errorf("bob total received: got %d, want >= 10000", got)
	}
}

// TestRecurrentTransferConsecutiveFailureDeletion ports
// operation_time_tests.cpp::recurrent_transfer_consecutive_failure_deletion.
// After 10 consecutive failures (insufficient balance), the row is
// removed automatically.
func TestRecurrentTransferConsecutiveFailureDeletion(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF25)
	f.Actors("alice", "bob")
	// Fund alice with just enough for ONE transfer; subsequent firings
	// will fail.
	f.FundLiquid("alice", 5_000)
	f.GenerateBlock()

	if err := f.ApplyOp(op.RecurrentTransfer{
		From: "alice", To: "bob", Amount: HIVE(5_000),
		Recurrence: 24, Executions: 100, Memo: "drain",
	}); err != nil {
		t.Fatalf("recurrent_transfer: %v", err)
	}
	// First fire succeeds, drains alice.
	f.SkipSeconds(24 * 3600)
	if got := f.Balance("alice"); got != 0 {
		t.Errorf("alice balance after first fire: got %d, want 0", got)
	}

	// Walk forward enough recurrences for 10 consecutive failures —
	// the fast path skips per-block hooks, so we need to fire the
	// schedule once per recurrence boundary.
	for i := 0; i < 12; i++ {
		f.SkipSeconds(24 * 3600)
	}

	s := state.NewSession(f.State)
	if _, err := objects.GetRecurrentTransfer(s, "alice", "bob", 0); err == nil {
		t.Errorf("recurrent_transfer still present after %d consecutive failures",
			10)
	}
}
