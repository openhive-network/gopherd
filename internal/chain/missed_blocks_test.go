package chain

import (
	"runtime"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

func missedBlocksSession(t *testing.T) *state.Session {
	t.Helper()
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}
	s := state.NewSession(db)
	for _, name := range []auth.AccountName{"w1", "w2"} {
		objects.PutWitness(s, &objects.Witness{Owner: name, LastConfirmedBlockNum: 1, BlockSigningKey: [33]byte{0x02, 1}})
	}
	ws, err := objects.GetWitnessSchedule(s)
	if err != nil {
		t.Fatal(err)
	}
	ws.CurrentShuffledWitnesses = []auth.AccountName{"w1", "w2"}
	ws.NumScheduledWitnesses = 2
	objects.PutWitnessSchedule(s, &ws)
	return s
}

// TestUpdateWitnessMissedBlocksPerSlot pins hived's per-slot semantics
// (database_witness.cpp:290-305): every missed slot of another witness adds
// one to its total_missed and emits producer_missed, preceded by
// shutdown_witness on every such slot between HF14 and HF20 once the witness
// has not confirmed a block for a day.
func TestUpdateWitnessMissedBlocksPerSlot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hf       hardfork.Set
		shutdown bool
	}{
		{"HF20", hardfork.SetForBlock(hardfork.MainnetActivationBlock[hardfork.HF20] + 1), false},
		{"HF14", hardfork.SetForBlock(hardfork.MainnetActivationBlock[hardfork.HF14] + 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := missedBlocksSession(t)
			d := &Database{}
			const slots = 10
			updateWitnessMissedBlocks(s, "w2", slots, tc.hf, 1+hiveWitnessShutdownThresholdBlocks+5, d)

			w1, _ := objects.GetWitness(s, "w1")
			w2, _ := objects.GetWitness(s, "w2")
			if w1.TotalMissedBlocks != slots/2 || w2.TotalMissedBlocks != 0 {
				t.Fatalf("missed: w1 %d w2 %d, want %d and 0", w1.TotalMissedBlocks, w2.TotalMissedBlocks, slots/2)
			}
			if got := w1.BlockSigningKey != ([33]byte{}); got == tc.shutdown {
				t.Errorf("w1 signing key kept=%v, want shut down=%v", got, tc.shutdown)
			}
			var missed, shut int
			for _, v := range d.vops {
				switch v.(type) {
				case op.ProducerMissed:
					missed++
				case op.ShutdownWitness:
					shut++
				}
			}
			wantShut := 0
			if tc.shutdown {
				wantShut = slots / 2
			}
			if missed != slots/2 || shut != wantShut {
				t.Errorf("vops: %d producer_missed, %d shutdown_witness; want %d and %d", missed, shut, slots/2, wantShut)
			}
		})
	}
}

// TestUpdateWitnessMissedBlocksAllocation pins that a long gap costs memory
// per witness, not per slot. Writing the witness back on every slot made a
// 90-day gap (2.6M slots) allocate 3.6 GB, and the fixture test that skips a
// year of governance-vote expiry needed over 9 GB.
func TestUpdateWitnessMissedBlocksAllocation(t *testing.T) {
	s := missedBlocksSession(t)
	hf := hardfork.SetForBlock(hardfork.MainnetActivationBlock[hardfork.HF20] + 1)
	const slots = 500_000
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	updateWitnessMissedBlocks(s, "w2", slots, hf, 100, nil)
	runtime.ReadMemStats(&after)
	if w1, _ := objects.GetWitness(s, "w1"); w1.TotalMissedBlocks != slots/2 {
		t.Fatalf("w1 missed %d, want %d", w1.TotalMissedBlocks, slots/2)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Errorf("%d slots allocated %d MiB, want well under 16", slots, alloc>>20)
	}
}
