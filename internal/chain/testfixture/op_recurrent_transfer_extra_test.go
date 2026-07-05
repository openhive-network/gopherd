package testfixture

import (
	"fmt"
	"strings"
	"testing"

	"hived-go/internal/chain"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// This file ports the remaining recurrent_transfer cases from hived's
// libraries/tests/unit/tests/operation_tests.cpp. The simpler cases
// (validate, apply, hbd, max_open_transfers smoke, pair_id_basic,
// pair_id_crud update/delete branches) are already covered in:
//
//   - op_recurrent_transfer_test.go (pair_id_basic, pair_id_crud update,
//     pair_id_crud delete, hbd)
//   - op_recurrent_time_test.go     (expiration_full, consecutive_failure_deletion)
//   - op_apply_more_test.go         (max_open_transfers smoke)
//   - op_more_test.go               (recurrent_transfer_apply)
//
// What this file adds:
//
//   - failed_recurrent_transfer
//   - recurrent_transfer_pair_id_crud (CRUD recreate branch, full flow)
//   - recurrent_transfer_same_pair_id_different_receivers
//   - recurrent_transfer_exact_max
//   - recurrent_transfer_edit_overextended
//   - recurrent_transfer_full_edit_overextended
//   - recurrent_transfer_max_transfer_processed_per_block (cap=1000)
//   - extensions_in_virtual_operations_generated_after_recurrent_transfer
//     (fill_recurrent_transfer / failed_recurrent_transfer vops carry
//     pair_id, observed via chain.Database.LastBlockVOps)

const recurrentTransferMaxEndDays = 730 // HIVE_MAX_RECURRENT_TRANSFER_END_DATE
const recurrentTransferMaxRecurrenceH = recurrentTransferMaxEndDays * 24

// countRecurrentTransfers returns the number of rows in the
// RecurrentTransfer table — analogue of
// `db->get_index<recurrent_transfer_index, by_id>().size()`.
func countRecurrentTransfers(t *testing.T, f *Fixture) int {
	t.Helper()
	n := 0
	s := state.NewSession(f.State)
	_ = s.Range(objects.RecurrentTransferTable, state.PrimaryIndex, nil,
		func(_, _ []byte) (bool, error) {
			n++
			return true, nil
		})
	return n
}

// TestRecurrentTransferFailedRecurrentTransfer ports
// operation_tests.cpp::failed_recurrent_transfer.
//
// alice has just enough HBD for ONE transfer. After the schedule fires
// once successfully, the next firing fails (balance=0). The C++ test
// verifies:
//
//   - failure-path decrements remaining_executions
//   - on failure with remaining_executions==0, the row is removed
//   - on failure with remaining_executions>0 and consec_failures<max,
//     the row stays
//
// In Go we use recurrence=24 (the validator's hardcoded minimum) instead
// of recurrence=1 (which C++ enables via configuration_data); the
// schedule semantics are identical and the test still exercises both
// outcomes by varying executions = {2, 3}.
func TestRecurrentTransferFailedRecurrentTransfer(t *testing.T) {
	run := func(t *testing.T, from, to string, executions uint16, wantDeleted bool) {
		f := New(t)
		f.Actors(from, to)
		f.FundLiquidHBD(from, 7_000)
		f.GenerateBlock()
		f.HardforksUpTo(hardfork.HF25)

		if err := f.ApplyOp(op.RecurrentTransfer{
			From:       from,
			To:         to,
			Amount:     HBD(6_000),
			Memo:       "test ok",
			Recurrence: 24,
			Executions: executions,
		}); err != nil {
			t.Fatalf("recurrent_transfer create: %v", err)
		}

		// First firing happens in the same block (Trigger=head_block_time
		// at create time). It drains `from` to 1_000 HBD.
		if got := f.HBDBalance(from); got != 1_000 {
			t.Fatalf("after first fire: %s hbd_balance = %d, want 1000", from, got)
		}
		if got := f.HBDBalance(to); got != 6_000 {
			t.Fatalf("after first fire: %s hbd_balance = %d, want 6000", to, got)
		}

		rt, err := objects.GetRecurrentTransfer(state.NewSession(f.State), from, to, 0)
		if err != nil {
			t.Fatalf("schedule not present after first fire: %v", err)
		}
		if rt.ConsecutiveFailures != 0 {
			t.Errorf("consecutive_failures after success: got %d, want 0", rt.ConsecutiveFailures)
		}
		if rt.Executions != executions-1 {
			t.Errorf("remaining_executions after success: got %d, want %d",
				rt.Executions, executions-1)
		}

		// Advance one recurrence — second firing attempts and fails
		// (insufficient balance). With executions==2, the row should
		// now be removed (remaining_executions reached 0). With
		// executions==3, the row should remain (one execution still
		// scheduled).
		f.SkipSeconds(24 * 3600)
		size := countRecurrentTransfers(t, f)
		if wantSize := int(executions) - 2; size != wantSize {
			t.Errorf("schedule row count after failed fire: got %d, want %d",
				size, wantSize)
		}
		got := size == 0
		if got != wantDeleted {
			t.Errorf("schedule deleted: got %v, want %v", got, wantDeleted)
		}
	}

	t.Run("executions=2_deleted", func(t *testing.T) {
		run(t, "alice", "bob", 2, true)
	})
	t.Run("executions=3_kept", func(t *testing.T) {
		run(t, "carol", "dan", 3, false)
	})
}

// TestRecurrentTransferExtensionsInVirtualOps ports
// operation_tests.cpp::extensions_in_virtual_operations_generated_after_recurrent_transfer.
//
// Verifies that the per-block virtual ops (fill_recurrent_transfer /
// failed_recurrent_transfer) carry the same pair_id as the originating
// recurrent_transfer_operation. Hived's C++ test pulls them off the
// last-operations queue; Go records them on Database.vops (see
// block_vops.go) and exposes them via LastBlockVOps().
//
// C++ uses recurrence=1 (with a config-data override). Go's validator
// hardcodes the 24-hour minimum, so we use recurrence=24 and advance
// the test clock by 24h between the success and failure firings —
// the schedule semantics are identical.
func TestRecurrentTransferExtensionsInVirtualOps(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 7_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF25)

	const pairIDValue uint8 = 78

	if err := f.ApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     HBD(6_000),
		Memo:       "test ok",
		Recurrence: 24,
		Executions: 2,
		Extensions: recurrentTransferPairIDExt(pairIDValue),
	}); err != nil {
		t.Fatalf("recurrent_transfer create: %v", err)
	}

	// First firing happens inside the create block (Trigger=head_block_time).
	// LastBlockVOps should contain exactly one fill_recurrent_transfer
	// vop carrying pair_id = 78.
	vops := f.Chain.LastBlockVOps()
	if len(vops) != 1 {
		t.Fatalf("after first fire: vops count = %d, want 1; got %+v", len(vops), vops)
	}
	if vops[0].Kind != chain.BlockVOpFillRecurrentTransfer {
		t.Errorf("after first fire: vop.Kind = %d, want BlockVOpFillRecurrentTransfer", vops[0].Kind)
	}
	if vops[0].PairID != pairIDValue {
		t.Errorf("after first fire: vop.PairID = %d, want %d", vops[0].PairID, pairIDValue)
	}
	if vops[0].From != "alice" || vops[0].To != "bob" {
		t.Errorf("after first fire: from=%q to=%q, want alice/bob", vops[0].From, vops[0].To)
	}
	if vops[0].Amount.Amount != 6_000 || vops[0].Amount.Symbol.Num != asset.HbdSymbol {
		t.Errorf("after first fire: amount = %+v, want 6000 HBD", vops[0].Amount)
	}
	if vops[0].RemainingExecutions != 1 {
		t.Errorf("after first fire: remaining = %d, want 1", vops[0].RemainingExecutions)
	}

	// Advance one recurrence — second firing fails (alice now has only
	// 1_000 HBD, can't transfer 6_000). Should produce one
	// failed_recurrent_transfer vop with the same pair_id.
	f.SkipSeconds(24 * 3600)
	vops = f.Chain.LastBlockVOps()
	if len(vops) != 1 {
		t.Fatalf("after second fire: vops count = %d, want 1; got %+v", len(vops), vops)
	}
	if vops[0].Kind != chain.BlockVOpFailedRecurrentTransfer {
		t.Errorf("after second fire: vop.Kind = %d, want BlockVOpFailedRecurrentTransfer", vops[0].Kind)
	}
	if vops[0].PairID != pairIDValue {
		t.Errorf("after second fire: vop.PairID = %d, want %d", vops[0].PairID, pairIDValue)
	}
	if vops[0].ConsecutiveFailures != 1 {
		t.Errorf("after second fire: consec_failures = %d, want 1", vops[0].ConsecutiveFailures)
	}
	// executions was 1 before this fire, so remaining = 0; that puts the
	// schedule into the deleted-on-failure branch.
	if vops[0].RemainingExecutions != 0 {
		t.Errorf("after second fire: remaining = %d, want 0", vops[0].RemainingExecutions)
	}
	if !vops[0].Deleted {
		t.Errorf("after second fire: vop.Deleted = false, want true (executions reached 0)")
	}
}

// TestRecurrentTransferExactMax ports operation_tests.cpp::recurrent_transfer_exact_max.
//
// Hived's `get_final_trigger_date` = trigger_date + recurrence_hours *
// (executions - 1). The duration cap is HIVE_MAX_RECURRENT_TRANSFER_END_DATE
// (730 days). The accept case has final = head + 17520h = head + 730d
// (exactly at the cap). The reject case has final = head + 2503h * 7 =
// head + 17521h, one hour past the cap.
func TestRecurrentTransferExactMax(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 200_000)
	f.FundLiquid("bob", 200_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF25)

	// Accept: 2 executions, recurrence = 17520h = 730 days.
	// Final trigger date = head + 17520h*(2-1) = head + 730d. Exactly at cap.
	if err := f.ApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     HBD(10_000),
		Memo:       "test ok",
		Recurrence: uint16(recurrentTransferMaxRecurrenceH),
		Executions: 2,
	}); err != nil {
		t.Fatalf("exact-max schedule should succeed: %v", err)
	}

	// Reject: 8 executions, recurrence = 2503h.
	// Final = head + 2503h*7 = head + 17521h. One hour past the cap.
	err := f.ApplyOp(op.RecurrentTransfer{
		From:       "bob",
		To:         "alice",
		Amount:     HIVE(10_000),
		Memo:       "test fail",
		Recurrence: 2503,
		Executions: 8,
	})
	if err == nil {
		t.Fatal("over-cap schedule should fail")
	}
	if !strings.Contains(err.Error(), "too long") &&
		!strings.Contains(err.Error(), "24-month") &&
		!strings.Contains(err.Error(), "cap") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRecurrentTransferEditOverextended ports
// operation_tests.cpp::recurrent_transfer_edit_overextended.
//
// Creating a schedule at exactly the max-duration cap is fine.
// Editing it without changing recurrence (just an amount tweak) is
// also fine — the existing trigger_date is preserved, so the final
// trigger remains at the original boundary. But editing AFTER a
// firing has happened reverts remaining_executions to the op's
// `executions` field; with original trigger preserved, the new final
// trigger would extend past head+cap.
func TestRecurrentTransferEditOverextended(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 200_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF25)

	// Both create and edit go into the SAME block (mirroring the C++
	// test which does two push_transaction calls then one generate_block).
	// That way the edit happens BEFORE the first firing has occurred,
	// trigger is preserved at head_block_time, and the schedule still
	// sits exactly at the duration cap.
	f.PushOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     HBD(1_000),
		Memo:       "test ok",
		Recurrence: uint16(recurrentTransferMaxRecurrenceH),
		Executions: 2,
	})
	f.PushOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     HBD(10_000),
		Memo:       "test ok",
		Recurrence: uint16(recurrentTransferMaxRecurrenceH),
		Executions: 2,
	})
	f.GenerateBlock()

	// At this point the first firing has happened (Trigger=head fires
	// in the same block); remaining_executions = 1.

	// Wait one more block then try a no-recurrence-change edit with
	// executions=2 again — this resets remaining_executions to 2, but
	// trigger has already advanced by recurrence hours after the first
	// fire. final = trigger + recurrence*(2-1) = old_head + 2*recurrence,
	// which is 1 cap-width past head_now. Should fail.
	f.SkipSeconds(3600) // 1 hour
	err := f.ApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     HBD(10_000),
		Memo:       "test fail",
		Recurrence: uint16(recurrentTransferMaxRecurrenceH),
		Executions: 2,
	})
	if err == nil {
		t.Fatal("overextended edit (after first fire) should fail")
	}
	if !strings.Contains(err.Error(), "too long") &&
		!strings.Contains(err.Error(), "cap") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRecurrentTransferFullEditOverextended ports
// operation_tests.cpp::recurrent_transfer_full_edit_overextended.
//
// Like edit_overextended but the edit CHANGES recurrence — that resets
// trigger_date to head + new_recurrence. The new final trigger date
// pushes past the cap, regardless of whether any fire has happened.
func TestRecurrentTransferFullEditOverextended(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 200_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF25)

	// Short-duration create (1 day, 2 execs).
	if err := f.ApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     HBD(10_000),
		Memo:       "test ok",
		Recurrence: 24,
		Executions: 2,
	}); err != nil {
		t.Fatalf("initial create: %v", err)
	}

	// Edit with new recurrence = max (730 days) and 2 executions.
	// Because recurrence changed, trigger resets to head + 730d.
	// Final = head + 730d * (2-1) + something — actually:
	// trigger = head + recurrence_h*3600, final = trigger +
	// recurrence_h*(executions-1)*3600 = head + 730d + 730d. That's
	// 2 cap-widths past head. Should fail.
	err := f.ApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     HBD(10_000),
		Memo:       "test fail",
		Recurrence: uint16(recurrentTransferMaxRecurrenceH),
		Executions: 2,
	})
	if err == nil {
		t.Fatal("overextended full edit should fail (immediately)")
	}
	if !strings.Contains(err.Error(), "too long") &&
		!strings.Contains(err.Error(), "cap") {
		t.Errorf("unexpected error: %v", err)
	}

	// Generate one block (3s only, not a full recurrence) — the original
	// recurrence=24 schedule has remaining_executions=1 still. The
	// overextended edit should still fail.
	f.GenerateBlock()
	err = f.ApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     HBD(10_000),
		Memo:       "test fail",
		Recurrence: uint16(recurrentTransferMaxRecurrenceH),
		Executions: 2,
	})
	if err == nil {
		t.Fatal("overextended full edit should still fail after one block")
	}
}

// TestRecurrentTransferMaxPerBlockProcessed ports
// operation_tests.cpp::recurrent_transfer_max_transfer_processed_per_block.
//
// Hived caps process_recurrent_transfers at
// HIVE_MAX_RECURRENT_TRANSFERS_PER_BLOCK = 1000 due rows per block.
// Excess rows defer to the next block. The C++ test creates 251 actors
// + 4 senders + 1004 schedules, generates one block (expects 1000 fires)
// then another (expects the remaining 4 fires).
//
// The Go port mirrors the setup but doesn't depend on hived's row-id
// tie-break — it just asserts the global invariant:
//
//   - exactly 1000 schedules have fired in block 1
//   - exactly 1004 schedules have fired by the end of block 2
//     (i.e. the 4 deferred ones caught up)
func TestRecurrentTransferMaxPerBlockProcessed(t *testing.T) {
	if testing.Short() {
		t.Skip("creates 255 actors + 1004 recurrent_transfers; too slow for -short")
	}

	f := New(t)
	f.Actors("alice", "bob", "eve", "martin")
	// 251 receivers: actor0..actor250.
	receivers := make([]string, 251)
	for i := 0; i < 251; i++ {
		receivers[i] = fmt.Sprintf("actor%d", i)
	}
	f.Actors(receivers...)
	for _, sender := range []string{"alice", "bob", "eve", "martin"} {
		f.FundLiquid(sender, 1_000_000) // 1000.000 HIVE
	}
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF25)

	const initialExecutions uint16 = 25

	// 1000 schedules: each of 4 senders × actor0..actor249, amount = 1 HIVE.
	for _, sender := range []string{"alice", "bob", "eve", "martin"} {
		for i := 0; i < 250; i++ {
			f.PushOp(op.RecurrentTransfer{
				From:       sender,
				To:         receivers[i],
				Amount:     HIVE(1_000),
				Memo:       "test",
				Recurrence: 24,
				Executions: initialExecutions,
			})
		}
	}
	// 4 extra schedules: each sender → actor250, amount = 3 HIVE.
	// Total = 1004, cap = 1000 → 4 deferred to block 2.
	for _, sender := range []string{"alice", "bob", "eve", "martin"} {
		f.PushOp(op.RecurrentTransfer{
			From:       sender,
			To:         receivers[250],
			Amount:     HIVE(3_000),
			Memo:       "test",
			Recurrence: 24,
			Executions: initialExecutions,
		})
	}
	f.GenerateBlock()

	// Count schedules where Executions has dropped from initial (=
	// fired at least once) vs still equals initial (= not yet fired).
	// Total rows = 1004; cap = 1000 → expect 1000 fired, 4 untouched.
	fired1, pending1 := countByExecutions(t, f, initialExecutions)
	if fired1 != 1000 {
		t.Errorf("after block 1: fired = %d, want 1000 (cap)", fired1)
	}
	if pending1 != 4 {
		t.Errorf("after block 1: pending = %d, want 4 (excess deferred)", pending1)
	}

	// Block 2 (3s later, far less than 24h recurrence) — the deferred
	// 4 rows still have trigger <= head_block_time, so they fire now.
	f.GenerateBlock()
	fired2, pending2 := countByExecutions(t, f, initialExecutions)
	if fired2 != 1004 {
		t.Errorf("after block 2: fired = %d, want 1004 (all rows fired exactly once)", fired2)
	}
	if pending2 != 0 {
		t.Errorf("after block 2: pending = %d, want 0", pending2)
	}
}

// countByExecutions buckets rows by whether Executions has dropped from
// initial (= fired at least once) or still equals initial (= never
// fired). Returns (fired, pending).
func countByExecutions(t *testing.T, f *Fixture, initial uint16) (fired, pending int) {
	t.Helper()
	s := state.NewSession(f.State)
	_ = s.Range(objects.RecurrentTransferTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			rt, err := objects.ReadRecurrentTransfer(value)
			if err != nil {
				return true, nil
			}
			if rt.Executions == initial {
				pending++
			} else {
				fired++
			}
			return true, nil
		})
	return fired, pending
}

// TestRecurrentTransferPairIDCRUDFull ports the full
// operation_tests.cpp::recurrent_transfer_pair_id_crud flow including
// the recreate-after-delete branch. The update and delete branches are
// already covered in op_recurrent_transfer_test.go but the recreate
// path was missing.
func TestRecurrentTransferPairIDCRUDFull(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 200_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF25)

	// Create: pair_id=0
	f.MustApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     asset.Asset{Amount: 10_000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Memo:       "test1",
		Recurrence: 72,
		Executions: 5,
		Extensions: recurrentTransferPairIDExt(0),
	})

	// Create: pair_id=100
	f.MustApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     asset.Asset{Amount: 20_000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Memo:       "test2",
		Recurrence: 48,
		Executions: 3,
		Extensions: recurrentTransferPairIDExt(100),
	})

	// Update pair_id=100
	f.MustApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     asset.Asset{Amount: 15_000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Memo:       "updated_test2",
		Recurrence: 60,
		Executions: 4,
		Extensions: recurrentTransferPairIDExt(100),
	})

	rt1, err := objects.GetRecurrentTransfer(state.NewSession(f.State), "alice", "bob", 0)
	if err != nil {
		t.Fatalf("read pair_id=0: %v", err)
	}
	if rt1.Memo != "test1" || rt1.Amount != 10_000 || rt1.Recurrence != 72 {
		t.Errorf("pair_id=0 unchanged: got memo=%q amount=%d rec=%d",
			rt1.Memo, rt1.Amount, rt1.Recurrence)
	}

	rt2, err := objects.GetRecurrentTransfer(state.NewSession(f.State), "alice", "bob", 100)
	if err != nil {
		t.Fatalf("read pair_id=100 after update: %v", err)
	}
	if rt2.Memo != "updated_test2" || rt2.Amount != 15_000 || rt2.Recurrence != 60 {
		t.Errorf("pair_id=100 updated: got memo=%q amount=%d rec=%d",
			rt2.Memo, rt2.Amount, rt2.Recurrence)
	}

	// Delete pair_id=100 (amount=0 on an existing schedule = cancel;
	// executions must be >= 2 pre-HF29, mirroring hived)
	f.MustApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     asset.Asset{Amount: 0, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Recurrence: 60,
		Executions: 2,
		Extensions: recurrentTransferPairIDExt(100),
	})

	if has, _ := objects.HasRecurrentTransfer(state.NewSession(f.State), "alice", "bob", 100); has {
		t.Errorf("pair_id=100 not deleted")
	}
	if has, _ := objects.HasRecurrentTransfer(state.NewSession(f.State), "alice", "bob", 0); !has {
		t.Errorf("pair_id=0 should still exist after deleting pair_id=100")
	}

	// Recreate pair_id=100 with different parameters.
	f.MustApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     asset.Asset{Amount: 8_000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Memo:       "new_test",
		Recurrence: 36,
		Executions: 6,
		Extensions: recurrentTransferPairIDExt(100),
	})

	rt3, err := objects.GetRecurrentTransfer(state.NewSession(f.State), "alice", "bob", 100)
	if err != nil {
		t.Fatalf("read pair_id=100 after recreate: %v", err)
	}
	if rt3.Memo != "new_test" || rt3.Amount != 8_000 || rt3.Recurrence != 36 {
		t.Errorf("pair_id=100 recreated: got memo=%q amount=%d rec=%d",
			rt3.Memo, rt3.Amount, rt3.Recurrence)
	}
	// First firing happens in the same apply block, so remaining
	// executions = 6 - 1 = 5 (mirrors C++ comment "since it was new
	// r.transfer (just reusing previously used id), we have one
	// execution already made").
	if rt3.Executions != 5 {
		t.Errorf("pair_id=100 recreated executions: got %d, want 5", rt3.Executions)
	}
}

// TestRecurrentTransferSamePairIDDifferentReceivers ports
// operation_tests.cpp::recurrent_transfer_same_pair_id_different_receivers.
// Two schedules from alice to different receivers can both use the
// same pair_id — the key is (from, to, pair_id).
func TestRecurrentTransferSamePairIDDifferentReceivers(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob", "dave")
	f.FundLiquidHBD("alice", 300_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF25)

	if err := f.ApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     asset.Asset{Amount: 10_000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Memo:       "test1",
		Recurrence: 72,
		Executions: 5,
		Extensions: recurrentTransferPairIDExt(1),
	}); err != nil {
		t.Fatalf("alice→bob pair_id=1: %v", err)
	}

	if err := f.ApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "dave",
		Amount:     asset.Asset{Amount: 20_000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Memo:       "test2",
		Recurrence: 48,
		Executions: 3,
		Extensions: recurrentTransferPairIDExt(1),
	}); err != nil {
		t.Fatalf("alice→dave pair_id=1: %v", err)
	}

	s := state.NewSession(f.State)
	r1, err := objects.GetRecurrentTransfer(s, "alice", "bob", 1)
	if err != nil {
		t.Fatalf("read alice→bob pair_id=1: %v", err)
	}
	r2, err := objects.GetRecurrentTransfer(s, "alice", "dave", 1)
	if err != nil {
		t.Fatalf("read alice→dave pair_id=1: %v", err)
	}
	if r1.Memo != "test1" || r1.Amount != 10_000 || r1.Recurrence != 72 {
		t.Errorf("alice→bob: got memo=%q amount=%d rec=%d", r1.Memo, r1.Amount, r1.Recurrence)
	}
	if r2.Memo != "test2" || r2.Amount != 20_000 || r2.Recurrence != 48 {
		t.Errorf("alice→dave: got memo=%q amount=%d rec=%d", r2.Memo, r2.Amount, r2.Recurrence)
	}
}
