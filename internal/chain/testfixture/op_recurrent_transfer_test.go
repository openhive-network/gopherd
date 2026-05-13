package testfixture

import (
	"testing"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// recurrentTransferPairIDExt builds the extensions byte sequence for a
// recurrent_transfer op carrying a pair_id. Format:
//
//	[varint count=1][varint tag=1][uint8 pair_id]
func recurrentTransferPairIDExt(pairID uint8) []byte {
	return []byte{1, 1, pairID}
}

// TestRecurrentTransferPairIDBasic ports operation_tests.cpp::recurrent_transfer_pair_id_basic.
// Two recurrent transfers from alice → bob with different pair_ids must
// coexist as separate schedules, both keyed under (from, to, pair_id).
func TestRecurrentTransferPairIDBasic(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 200_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF25)

	// First schedule: pair_id 0, 10 HBD every 72h, 5 executions.
	if err := f.ApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     asset.Asset{Amount: 10_000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Memo:       "test1",
		Recurrence: 72,
		Executions: 5,
		Extensions: recurrentTransferPairIDExt(0),
	}); err != nil {
		t.Fatalf("recurrent_transfer #1: %v", err)
	}

	// Second schedule: pair_id 1, 20 HBD every 48h, 3 executions.
	if err := f.ApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     asset.Asset{Amount: 20_000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Memo:       "test2",
		Recurrence: 48,
		Executions: 3,
		Extensions: recurrentTransferPairIDExt(1),
	}); err != nil {
		t.Fatalf("recurrent_transfer #2: %v", err)
	}

	// Both schedules should exist independently.
	s := state.NewSession(f.State)
	rt0, err := objects.GetRecurrentTransfer(s, "alice", "bob", 0)
	if err != nil {
		t.Fatalf("GetRecurrentTransfer pair_id=0: %v", err)
	}
	// The first firing happens in the same apply block (Trigger =
	// head_block_time at create, processRecurrentTransfers fires before
	// commit), so remaining_executions = N - 1.
	if rt0.Memo != "test1" || rt0.Amount != 10_000 || rt0.Executions != 4 {
		t.Errorf("pair_id=0: got memo=%q amount=%d execs=%d, want test1/10000/4",
			rt0.Memo, rt0.Amount, rt0.Executions)
	}

	rt1, err := objects.GetRecurrentTransfer(s, "alice", "bob", 1)
	if err != nil {
		t.Fatalf("GetRecurrentTransfer pair_id=1: %v", err)
	}
	if rt1.Memo != "test2" || rt1.Amount != 20_000 || rt1.Executions != 2 {
		t.Errorf("pair_id=1: got memo=%q amount=%d execs=%d, want test2/20000/2",
			rt1.Memo, rt1.Amount, rt1.Executions)
	}
}

// TestRecurrentTransferPairIDCRUDUpdate ports the update branch of
// recurrent_transfer_pair_id_crud. Re-issuing an op with the same pair_id
// updates the existing schedule's fields rather than creating a new one.
func TestRecurrentTransferPairIDCRUDUpdate(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 200_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF25)

	// Initial create with pair_id=100, recurrence=48, execs=3, 20 HBD.
	f.MustApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     asset.Asset{Amount: 20_000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Memo:       "test",
		Recurrence: 48,
		Executions: 3,
		Extensions: recurrentTransferPairIDExt(100),
	})

	// Update: same pair_id, changed memo/amount/recurrence/executions.
	f.MustApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     asset.Asset{Amount: 15_000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Memo:       "updated",
		Recurrence: 60,
		Executions: 4,
		Extensions: recurrentTransferPairIDExt(100),
	})

	rt, err := objects.GetRecurrentTransfer(state.NewSession(f.State), "alice", "bob", 100)
	if err != nil {
		t.Fatalf("GetRecurrentTransfer: %v", err)
	}
	if rt.Memo != "updated" {
		t.Errorf("Memo: got %q, want updated", rt.Memo)
	}
	if rt.Amount != 15_000 {
		t.Errorf("Amount: got %d, want 15000", rt.Amount)
	}
	if rt.Recurrence != 60 {
		t.Errorf("Recurrence: got %d, want 60", rt.Recurrence)
	}
}

// TestRecurrentTransferPairIDCRUDDelete ports the delete branch of
// recurrent_transfer_pair_id_crud. Issuing an op with amount=0 and
// executions=0 deletes the schedule.
func TestRecurrentTransferPairIDCRUDDelete(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 200_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF25)

	f.MustApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     asset.Asset{Amount: 10_000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Memo:       "test",
		Recurrence: 72,
		Executions: 5,
		Extensions: recurrentTransferPairIDExt(7),
	})

	// Verify present.
	if has, _ := objects.HasRecurrentTransfer(state.NewSession(f.State), "alice", "bob", 7); !has {
		t.Fatal("expected schedule with pair_id=7 to exist")
	}

	// Delete via amount=0/executions=0.
	f.MustApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     asset.Asset{Amount: 0, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Recurrence: 72,
		Executions: 0,
		Extensions: recurrentTransferPairIDExt(7),
	})

	if has, _ := objects.HasRecurrentTransfer(state.NewSession(f.State), "alice", "bob", 7); has {
		t.Error("expected schedule with pair_id=7 to be deleted")
	}
}

// TestRecurrentTransferHBD ports operation_tests.cpp::recurrent_transfer_hbd.
// Verifies HBD asset is accepted (not just HIVE) and the schedule is
// correctly typed.
func TestRecurrentTransferHBD(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 100_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF25)

	if err := f.ApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     asset.Asset{Amount: 5_000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Recurrence: 24,
		Executions: 4,
	}); err != nil {
		t.Fatalf("HBD recurrent_transfer: %v", err)
	}

	rt, err := objects.GetRecurrentTransfer(state.NewSession(f.State), "alice", "bob", 0)
	if err != nil {
		t.Fatalf("GetRecurrentTransfer: %v", err)
	}
	if rt.Symbol != uint32(asset.HbdSymbol) {
		t.Errorf("Symbol: got %d, want HBD (%d)", rt.Symbol, asset.HbdSymbol)
	}
	if rt.Amount != 5_000 {
		t.Errorf("Amount: got %d, want 5000", rt.Amount)
	}
}
