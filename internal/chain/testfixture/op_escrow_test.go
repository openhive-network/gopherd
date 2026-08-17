package testfixture

import (
	"testing"

	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// Port of operation_tests.cpp::escrow_transfer_apply.
//
// alice escrows 1 HIVE + 1 HBD with bob (recipient) and sam (agent).
// Tests:
//   - failure: alice lacks HBD
//   - failure: alice lacks HIVE+fee
//   - failure: ratification deadline in past
//   - failure: expiration in past
//   - success: state mutations look correct, balances debited
func TestEscrowTransferApply(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob", "sam")
	f.GenerateBlock()
	f.FundLiquid("alice", 10_000) // 10 HIVE liquid; no HBD

	now := f.HeadBlockTime()

	t.Log("--- failure when alice lacks HBD")
	if err := f.ApplyOp(op.EscrowTransfer{
		From:                 "alice",
		To:                   "bob",
		Agent:                "sam",
		EscrowID:             0,
		HbdAmount:            HBD(1_000),
		HiveAmount:           HIVE(1_000),
		Fee:                  HIVE(100),
		RatificationDeadline: now + 100,
		EscrowExpiration:     now + 200,
	}); err == nil {
		t.Fatal("expected insufficient-HBD failure")
	}

	t.Log("--- failure when alice lacks HIVE + fee")
	if err := f.ApplyOp(op.EscrowTransfer{
		From:                 "alice",
		To:                   "bob",
		Agent:                "sam",
		EscrowID:             0,
		HbdAmount:            HBD(0),
		HiveAmount:           HIVE(10_000),
		Fee:                  HIVE(100),
		RatificationDeadline: now + 100,
		EscrowExpiration:     now + 200,
	}); err == nil {
		t.Fatal("expected insufficient HIVE+fee failure")
	}

	t.Log("--- failure when ratification deadline is in the past")
	if err := f.ApplyOp(op.EscrowTransfer{
		From:                 "alice",
		To:                   "bob",
		Agent:                "sam",
		EscrowID:             0,
		HiveAmount:           HIVE(1_000),
		Fee:                  HIVE(100),
		RatificationDeadline: now - 100,
		EscrowExpiration:     now + 200,
	}); err == nil {
		t.Fatal("expected past-ratification-deadline failure")
	}

	t.Log("--- failure when expiration is in the past")
	if err := f.ApplyOp(op.EscrowTransfer{
		From:                 "alice",
		To:                   "bob",
		Agent:                "sam",
		EscrowID:             0,
		HiveAmount:           HIVE(1_000),
		Fee:                  HIVE(100),
		RatificationDeadline: now + 100,
		EscrowExpiration:     now - 50,
	}); err == nil {
		t.Fatal("expected past-expiration failure")
	}

	t.Log("--- success: 1 HIVE + 0.1 fee debited; escrow exists")
	preAlice := f.Balance("alice")
	if err := f.ApplyOp(op.EscrowTransfer{
		From:                 "alice",
		To:                   "bob",
		Agent:                "sam",
		EscrowID:             0,
		HbdAmount:            HBD(0),
		HiveAmount:           HIVE(1_000),
		Fee:                  HIVE(100),
		RatificationDeadline: f.HeadBlockTime() + 100,
		EscrowExpiration:     f.HeadBlockTime() + 200,
	}); err != nil {
		t.Fatalf("escrow: %v", err)
	}
	if got := f.Balance("alice"); got != preAlice-1_100 {
		t.Fatalf("alice debit: got %d, want %d", got, preAlice-1_100)
	}
	s := state.NewSession(f.State)
	esc, err := objects.GetEscrow(s, "alice", 0)
	if err != nil {
		t.Fatalf("escrow lookup: %v", err)
	}
	if esc.From != "alice" || esc.To != "bob" || esc.Agent != "sam" {
		t.Fatal("escrow parties wrong")
	}
	if esc.HiveAmount != 1_000 {
		t.Fatalf("escrow HiveAmount: got %d, want 1000", esc.HiveAmount)
	}
	if esc.PendingFee != 100 {
		t.Fatalf("escrow Fee: got %d, want 100", esc.PendingFee)
	}
	if esc.ToApproved || esc.AgentApproved {
		t.Fatal("escrow should not be pre-approved")
	}
}
