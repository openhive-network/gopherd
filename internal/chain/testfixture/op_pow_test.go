package testfixture

import (
	"testing"

	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// validPowProps returns LegacyChainProperties with HIVE-symbol fee and
// non-zero block size — pow ops embed a chain_properties payload that
// the parser fully validates.
func validPowProps() op.LegacyChainProperties {
	return op.LegacyChainProperties{
		AccountCreationFee: LegacyHIVE(0),
		MaximumBlockSize:   65536,
		HBDInterestRate:    1000,
	}
}

// TestPowApplyCreatesAccountAndWitness ports operation_tests.cpp::pow_apply.
// hived's pow_apply (libraries/chain/hive_evaluator.cpp:454):
//   - creates the worker account if absent
//   - registers the worker as a witness if absent
//   - bumps dgp.num_pow_witnesses
//   - sets witness.pow_worker = head_block_num + 1 so the by_pow index
//     is monotonic
//   - pays the pow reward to dgp.current_witness (the BLOCK PRODUCER,
//     not the worker) — pre-HF13 only.
func TestPowApplyCreatesAccountAndWitness(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	// Apply a pow op for a fresh worker — account should not exist yet.
	if has, _ := objects.HasAccount(state.NewSession(f.State), "miner1"); has {
		t.Fatal("miner1 should not exist before pow op")
	}
	if has, _ := objects.HasWitness(state.NewSession(f.State), "miner1"); has {
		t.Fatal("miner1 witness should not exist before pow op")
	}

	if err := f.ApplyOp(op.Pow{
		WorkerAccount: "miner1",
		Props:         validPowProps(),
	}); err != nil {
		t.Fatalf("pow apply: %v", err)
	}

	// Account created.
	if has, _ := objects.HasAccount(state.NewSession(f.State), "miner1"); !has {
		t.Error("miner1 account should exist post pow")
	}
	// Witness created.
	if has, _ := objects.HasWitness(state.NewSession(f.State), "miner1"); !has {
		t.Error("miner1 witness should exist post pow")
	}
	// pow_worker > 0 (in by_pow index).
	w, _ := objects.GetWitness(state.NewSession(f.State), "miner1")
	if w.PowWorker == 0 {
		t.Error("miner1 PowWorker should be non-zero after pow")
	}
}

// TestPowApplyReturningWorkerOverwritesPowWorker verifies the C++ pow_apply
// path (hive_evaluator.cpp:451-467): a re-pow on an existing witness
// bumps dgp.total_pow + num_pow_witnesses and overwrites pow_worker
// with the post-bump total_pow value. (The C++ assertion at line 461
// is observed to be inert for back-to-back pow ops in the same block,
// so we don't enforce it here either; the canonical block_log applies
// such pairs cleanly.)
func TestPowApplyReturningWorkerOverwritesPowWorker(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	f.MustApplyOp(op.Pow{WorkerAccount: "miner1", Props: validPowProps()})
	w1, _ := objects.GetWitness(state.NewSession(f.State), "miner1")
	pow1 := w1.PowWorker
	if pow1 == 0 {
		t.Fatalf("first pow should leave pow_worker > 0")
	}
	f.MustApplyOp(op.Pow{WorkerAccount: "miner1", Props: validPowProps()})
	w2, _ := objects.GetWitness(state.NewSession(f.State), "miner1")
	if w2.PowWorker == pow1 {
		t.Errorf("second pow should have re-bumped pow_worker (still %d)", w2.PowWorker)
	}
	if w2.PowWorker <= pow1 {
		t.Errorf("pow_worker should monotonically increase: %d → %d", pow1, w2.PowWorker)
	}
}

// TestPow2ApplyCreatesAccountAndWitness verifies pow2 (HF13+ format) also
// creates account+witness for fresh worker. The pow2 op carries the
// worker name in PowInput (variant tag=0) or EquihashWork (variant tag=1).
func TestPow2ApplyCreatesAccountAndWitness(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	if err := f.ApplyOp(op.Pow2{
		WorkVariantTag: 0,
		PowInput: op.Pow2Input{
			WorkerAccount: "miner2",
		},
		Props: validPowProps(),
	}); err != nil {
		t.Fatalf("pow2 apply: %v", err)
	}

	if has, _ := objects.HasAccount(state.NewSession(f.State), "miner2"); !has {
		t.Error("miner2 account should exist post pow2")
	}
	if has, _ := objects.HasWitness(state.NewSession(f.State), "miner2"); !has {
		t.Error("miner2 witness should exist post pow2")
	}
}

// TestPowApplyEmptyWorkerRejected verifies that pow with an empty worker
// account name is rejected at evaluator level.
func TestPowApplyEmptyWorkerRejected(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	err := f.ApplyOp(op.Pow{WorkerAccount: ""})
	if err == nil {
		t.Fatal("pow with empty worker should fail")
	}
}

// TestPowBumpsNumPowWitnesses verifies that each pow op increments
// dgpo.NumPowWitnesses. This counter gates the pre-HIVE_START_VESTING_BLOCK
// reward (no payout until num >= 21).
func TestPowBumpsNumPowWitnesses(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	dgpo0 := f.DGPO()
	startNum := dgpo0.NumPowWitnesses

	for _, name := range []string{"m1", "m2", "m3"} {
		f.MustApplyOp(op.Pow{WorkerAccount: name, Props: validPowProps()})
	}

	dgpo1 := f.DGPO()
	if dgpo1.NumPowWitnesses != startNum+3 {
		t.Errorf("NumPowWitnesses: got %d, want %d (start=%d + 3)",
			dgpo1.NumPowWitnesses, startNum+3, startNum)
	}
}
