package testfixture

import (
	"testing"

	"gopherd/internal/protocol/op"
)

// TestFixtureBootstraps verifies the basic fixture can boot, create
// accounts, fund them, and survive a few empty blocks.
func TestFixtureBootstraps(t *testing.T) {
	f := New(t)
	if f.BlockNum != 0 {
		t.Fatalf("BlockNum at boot: got %d, want 0", f.BlockNum)
	}
	f.GenerateBlock()
	if f.BlockNum != 1 {
		t.Fatalf("BlockNum after one block: got %d, want 1", f.BlockNum)
	}
	f.Actor("alice")
	if !f.HasAccount("alice") {
		t.Fatal("alice should exist")
	}
	f.FundLiquid("alice", 1_000) // 1.000 HIVE
	if got := f.Balance("alice"); got != 1_000 {
		t.Fatalf("alice.Balance after FundLiquid: got %d, want 1000", got)
	}
}

// TestFixtureTransfer pushes a transfer through the apply pipeline.
func TestFixtureTransfer(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 1_000)
	f.GenerateBlock()

	if err := f.ApplyOp(op.Transfer{
		From:   "alice",
		To:     "bob",
		Amount: HIVE(500),
		Memo:   "hi",
	}); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	if got := f.Balance("alice"); got != 500 {
		t.Fatalf("alice.Balance: got %d, want 500", got)
	}
	if got := f.Balance("bob"); got != 500 {
		t.Fatalf("bob.Balance: got %d, want 500", got)
	}
}

// TestFixtureTransferInsufficient expects a failing transfer.
func TestFixtureTransferInsufficient(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 100)
	f.GenerateBlock()

	err := f.ApplyOp(op.Transfer{
		From:   "alice",
		To:     "bob",
		Amount: HIVE(500),
	})
	if err == nil {
		t.Fatal("expected insufficient-balance failure, got nil")
	}
}

// TestFixtureFundVesting confirms vest helper credits VESTS to the
// recipient at the genesis 1:1000 ratio.
func TestFixtureFundVesting(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.FundVesting("alice", 1_000) // 1 HIVE → 1000 VESTS-sat
	want := int64(1_000 * 1000)
	if got := f.Vests("alice"); got != want {
		t.Fatalf("alice.Vests: got %d, want %d", got, want)
	}
}
