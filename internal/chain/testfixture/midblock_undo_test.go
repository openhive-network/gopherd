package testfixture

import (
	"testing"

	"gopherd/internal/protocol/op"
)

// Port of undo_tests.cpp idiom: a transaction that fails mid-flight
// must NOT commit any of its earlier ops. Hived's apply_block wraps
// every push_transaction in a chainbase undo session that rolls back
// on throw. We mirror that with state.Session.Snapshot/Restore.
//
// All three ops land in ONE tx (PushOps before one GenerateBlock).
// First op debits alice→bob 100 (would succeed). Second op debits
// alice→bob 99,999 (would fail — insufficient). The whole tx must
// roll back; alice/bob balances unchanged.
func TestMidBlockTxAtomicityIntraTx(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 1_000)
	f.GenerateBlock()

	preAlice := f.Balance("alice")
	preBob := f.Balance("bob")

	f.PushOp(op.Transfer{
		From: "alice", To: "bob", Amount: HIVE(100),
	})
	f.PushOp(op.Transfer{
		From: "alice", To: "bob", Amount: HIVE(99_999),
	})
	if err := f.tryGenerateBlock(); err == nil {
		t.Fatal("expected the tx to fail on the second op")
	}

	if got := f.Balance("alice"); got != preAlice {
		t.Fatalf("alice's first-op debit leaked through: pre=%d post=%d",
			preAlice, got)
	}
	if got := f.Balance("bob"); got != preBob {
		t.Fatalf("bob's first-op credit leaked through: pre=%d post=%d",
			preBob, got)
	}
}

// Single-op tx that fails leaves no state change.
func TestMidBlockSingleOpTxFailureNoStateChange(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 100)
	f.GenerateBlock()

	preAlice := f.Balance("alice")
	preBob := f.Balance("bob")

	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "bob", Amount: HIVE(1_000_000),
	}); err == nil {
		t.Fatal("expected insufficient-balance failure")
	}

	if got := f.Balance("alice"); got != preAlice {
		t.Fatalf("alice's balance leaked through: pre=%d post=%d", preAlice, got)
	}
	if got := f.Balance("bob"); got != preBob {
		t.Fatalf("bob's balance leaked through: pre=%d post=%d", preBob, got)
	}
}

// Mid-tx witness vote → comment: if vote fails, no comment writes
// from a follow-up succeed.
func TestMidBlockMixedOpFailureRollsBackAll(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.GenerateBlock()

	// First op: post a comment (succeeds).
	// Second op: vote on a non-existent comment (fails).
	// The whole tx rolls back, so alice's posted comment vanishes.
	f.PushOp(op.Comment{
		Author: "alice", Permlink: "p", Body: "hi",
	})
	f.PushOp(op.Vote{
		Voter: "alice", Author: "bob", Permlink: "ghost", Weight: 10000,
	})
	if err := f.tryGenerateBlock(); err == nil {
		t.Fatal("expected the second op to fail")
	}
	if f.HasComment("alice", "p") {
		t.Fatal("alice's comment should have been rolled back with the tx")
	}
}

// Successful ops after a rolled-back tx should still apply normally.
// Sanity check that Restore doesn't poison subsequent activity.
func TestMidBlockSuccessAfterRollback(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 1_000)
	f.GenerateBlock()

	// First tx: insufficient → rolls back.
	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "bob", Amount: HIVE(99_999),
	}); err == nil {
		t.Fatal("expected failure")
	}
	// Second tx: legitimate transfer → succeeds.
	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "bob", Amount: HIVE(100),
	}); err != nil {
		t.Fatalf("legitimate transfer should succeed: %v", err)
	}
	if f.Balance("alice") != 900 {
		t.Fatalf("alice: got %d, want 900", f.Balance("alice"))
	}
	if f.Balance("bob") != 100 {
		t.Fatalf("bob: got %d, want 100", f.Balance("bob"))
	}
}
