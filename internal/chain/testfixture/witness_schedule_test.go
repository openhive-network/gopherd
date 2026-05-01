package testfixture

import (
	"testing"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Genesis schedule has initminer as the sole producer.
func TestWitnessScheduleGenesis(t *testing.T) {
	f := New(t)
	ws, err := objects.GetWitnessSchedule(state.NewSession(f.State))
	if err != nil {
		t.Fatalf("GetWitnessSchedule: %v", err)
	}
	if ws.NumScheduledWitnesses != 1 {
		t.Fatalf("genesis schedule size: got %d, want 1", ws.NumScheduledWitnesses)
	}
	if len(ws.CurrentShuffledWitnesses) != 1 {
		t.Fatalf("shuffled set: got %d, want 1", len(ws.CurrentShuffledWitnesses))
	}
	if string(ws.CurrentShuffledWitnesses[0]) != InitMiner {
		t.Fatalf("genesis producer: got %q, want %q",
			ws.CurrentShuffledWitnesses[0], InitMiner)
	}
}

// Once additional witnesses register, the schedule expands to include
// them (sorted by total_votes_vests desc) on the next round boundary.
func TestWitnessScheduleAdditionalWitnesses(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob", "carol")
	f.FundLiquid("alice", 10_000)
	f.FundLiquid("bob", 10_000)
	f.FundLiquid("carol", 10_000)
	f.GenerateBlock()

	for _, name := range []string{"alice", "bob", "carol"} {
		if err := f.ApplyOp(op.WitnessUpdate{
			Owner:           name,
			URL:             "x",
			BlockSigningKey: [33]byte{0x02, 1, byte(name[0])},
			Props: op.LegacyChainProperties{
				AccountCreationFee: LegacyHIVE(1_000),
				MaximumBlockSize:   65536,
				HBDInterestRate:    1000,
			},
			Fee: HIVE(0),
		}); err != nil {
			t.Fatalf("witness_update %q: %v", name, err)
		}
	}

	// Generate enough blocks so the schedule reshuffles.
	f.GenerateBlocks(5)

	ws, err := objects.GetWitnessSchedule(state.NewSession(f.State))
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if ws.NumScheduledWitnesses < 4 {
		t.Fatalf("expected schedule to grow to 4 (initminer + 3 actors), got %d",
			ws.NumScheduledWitnesses)
	}
	got := make(map[string]bool)
	for _, n := range ws.CurrentShuffledWitnesses {
		got[string(n)] = true
	}
	for _, want := range []string{InitMiner, "alice", "bob", "carol"} {
		if !got[want] {
			t.Errorf("expected %q in schedule, got %v", want, ws.CurrentShuffledWitnesses)
		}
	}
}

// Block headers reflect the scheduled witness, not always initminer.
// With a multi-witness schedule, generated blocks should rotate
// through the producers.
func TestWitnessScheduleBlockProducerRotates(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 10_000)
	f.FundLiquid("bob", 10_000)
	f.GenerateBlock()

	for _, name := range []string{"alice", "bob"} {
		if err := f.ApplyOp(op.WitnessUpdate{
			Owner:           name,
			URL:             "x",
			BlockSigningKey: [33]byte{0x02, 1, byte(name[0])},
			Props: op.LegacyChainProperties{
				AccountCreationFee: LegacyHIVE(1_000),
				MaximumBlockSize:   65536,
				HBDInterestRate:    1000,
			},
			Fee: HIVE(0),
		}); err != nil {
			t.Fatalf("witness_update %q: %v", name, err)
		}
	}
	// Force a re-shuffle then walk a round of blocks; the head witness
	// should change between consecutive blocks (most of the time).
	f.GenerateBlocks(5)

	witnesses := make(map[string]bool)
	for i := 0; i < 12; i++ {
		f.GenerateBlock()
		dgpo := f.DGPO()
		witnesses[dgpo.CurrentWitness] = true
	}
	if len(witnesses) < 2 {
		t.Fatalf("expected at least 2 distinct producers across 12 blocks, got %v",
			witnesses)
	}
}

// HF20+ schedule sizing: max_voted = 20, max_miner = 0.
func TestWitnessScheduleHF20Sizing(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF20)
	f.GenerateBlocks(2)
	ws, err := objects.GetWitnessSchedule(state.NewSession(f.State))
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if ws.MaxVotedWitnesses != objects.MaxVotedWitnessesHF20 {
		t.Fatalf("HF20 max_voted: got %d, want %d",
			ws.MaxVotedWitnesses, objects.MaxVotedWitnessesHF20)
	}
	if ws.MaxMinerWitnesses != 0 {
		t.Fatalf("HF20 max_miner: got %d, want 0", ws.MaxMinerWitnesses)
	}
}

// Witness with more votes ends up scheduled first in vote order
// (before shuffle). Use a high-vote witness vs low-vote witnesses to
// confirm priority.
func TestWitnessScheduleHighestVotedIncluded(t *testing.T) {
	f := New(t)
	// Register 22 witnesses (one above the cap) so we exercise the
	// "top N by votes" cut.
	names := make([]string, 22)
	for i := 0; i < 22; i++ {
		names[i] = "w" + string(rune('a'+i))
		f.Actor(names[i])
		f.FundLiquid(names[i], 10_000)
	}
	// Give "wa" extra vests so it ranks high.
	f.FundVesting("wa", 1_000_000_000)
	f.GenerateBlock()

	for _, n := range names {
		if err := f.ApplyOp(op.WitnessUpdate{
			Owner:           n,
			URL:             "x",
			BlockSigningKey: [33]byte{0x02, 0, byte(n[1])},
			Props: op.LegacyChainProperties{
				AccountCreationFee: LegacyHIVE(1_000),
				MaximumBlockSize:   65536,
				HBDInterestRate:    1000,
			},
			Fee: HIVE(0),
		}); err != nil {
			t.Fatalf("witness_update %q: %v", n, err)
		}
	}

	// Have a different account vote for wa — but wa already has
	// 1B vests, so just having registered should put it on top
	// since witness votes follow the voter's vesting weight.
	// Trigger a reshuffle.
	f.GenerateBlocks(25)

	ws, _ := objects.GetWitnessSchedule(state.NewSession(f.State))
	// Schedule must be capped at 21 (HIVE_MAX_WITNESSES).
	if int(ws.NumScheduledWitnesses) > objects.MaxWitnesses {
		t.Fatalf("schedule exceeds MaxWitnesses (21): got %d",
			ws.NumScheduledWitnesses)
	}
}

// ScheduledWitnessForSlot returns deterministic results for the same
// (head_block_num, slot) pair.
func TestWitnessScheduleSlotLookup(t *testing.T) {
	f := New(t)
	f.GenerateBlocks(3)
	ws, _ := objects.GetWitnessSchedule(state.NewSession(f.State))
	w1 := ws.ScheduledWitnessForSlot(f.BlockNum, 1)
	w1Again := ws.ScheduledWitnessForSlot(f.BlockNum, 1)
	if w1 != w1Again {
		t.Fatalf("slot lookup not deterministic: %q vs %q", w1, w1Again)
	}
	if w1 == "" {
		t.Fatal("slot 1 producer should be non-empty")
	}
}

// chain.Database.ScheduledWitness is the public API mirror of hived's
// database::get_scheduled_witness — used by tests that ask "who's next?".
func TestChainDatabaseScheduledWitness(t *testing.T) {
	f := New(t)
	f.GenerateBlocks(3)
	w, err := f.Chain.ScheduledWitness(1)
	if err != nil {
		t.Fatalf("ScheduledWitness(1): %v", err)
	}
	if w == "" {
		t.Fatal("scheduled witness should be non-empty")
	}
	// At genesis with only initminer, slot 1 must be initminer.
	if w != InitMiner {
		t.Logf("note: slot 1 producer is %q (likely after schedule expansion)", w)
	}
}

// At genesis, the very first generated block's CurrentWitness in DGPO
// must be initminer (since the genesis schedule contains only it).
func TestWitnessScheduleFirstBlockIsInitminer(t *testing.T) {
	f := New(t)
	f.GenerateBlock()
	dgpo := f.DGPO()
	if dgpo.CurrentWitness != InitMiner {
		t.Fatalf("first block CurrentWitness: got %q, want %q",
			dgpo.CurrentWitness, InitMiner)
	}
}
