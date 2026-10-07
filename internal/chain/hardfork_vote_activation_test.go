package chain

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// TestVotedHardforkActivatesLikeHived pins database_hardfork.cpp:121-133 for
// a fork with no compiled-in block (HF29): once the witness-voted
// (next_hardfork, next_hardfork_time) is reached, the fork applies at the
// end of that block, the block is recorded in the hardfork_property_object,
// and SetForBlock reports it active from the NEXT block on. Before this,
// HF29 was pinned to NeverActivates and could never fire.
func TestVotedHardforkActivatesLikeHived(t *testing.T) {
	if hardfork.IsScheduled(hardfork.HF29) {
		t.Skip("HF29 has a compiled-in activation block in this build")
	}
	t.Cleanup(func() { hardfork.ClearDynamicActivation(hardfork.HF29) })

	const votedTime uint32 = 1_800_000_000
	const blockNum uint32 = 120_000_000 // well past HF28, so HF5 rules apply
	s := state.NewSession(state.OpenMem())
	objects.EnsureDGPOStore(s)
	objects.PutDGPO(s, &objects.DynamicGlobalProperty{HeadBlockNumber: blockNum - 1, Time: votedTime - 3})
	objects.PutHardforkPropertyObject(s, &objects.HardforkPropertyObject{
		ProcessedHardforks:     make([]uint32, 29),
		LastHardfork:           28,
		CurrentHardforkVersion: hardfork.PackedVersion(hardfork.HF28),
		NextHardfork:           hardfork.PackedVersion(hardfork.HF29),
		NextHardforkTime:       votedTime,
	})

	// One block before the voted time: nothing happens.
	processHardforks(s, blockNum-1, votedTime-3, nil)
	hpo, _ := objects.GetHardforkPropertyObject(s)
	if hpo.LastHardfork != 28 {
		t.Fatalf("HF29 applied before next_hardfork_time (last_hardfork=%d)", hpo.LastHardfork)
	}
	if hardfork.IsScheduled(hardfork.HF29) {
		t.Fatal("activation table changed before the fork fired")
	}

	// The block at the voted time applies it.
	processHardforks(s, blockNum, votedTime, nil)
	hpo, _ = objects.GetHardforkPropertyObject(s)
	if hpo.LastHardfork != 29 {
		t.Fatalf("HF29 not applied at next_hardfork_time (last_hardfork=%d)", hpo.LastHardfork)
	}
	if hpo.CurrentHardforkVersion != hardfork.PackedVersion(hardfork.HF29) {
		t.Fatalf("current_hardfork_version = %#x, want %#x", hpo.CurrentHardforkVersion, hardfork.PackedVersion(hardfork.HF29))
	}
	if got := hpo.ProcessedHardforks[29]; got != votedTime {
		t.Fatalf("processed_hardforks[29] = %d, want the voted time %d (hived records HIVE_HARDFORK_1_29_TIME)", got, votedTime)
	}
	if b, ok := hpo.DynamicActivationBlock(29); !ok || b != blockNum {
		t.Fatalf("dynamic activation block = %d,%v, want %d", b, ok, blockNum)
	}
	// hived: has_hardfork(HF29) is false during block N's evaluators and true
	// from N+1; gopherd's SetForBlock uses the same strict `>`.
	if hardfork.SetForBlock(blockNum).Has(hardfork.HF29) {
		t.Fatal("HF29 reported active for the activation block itself")
	}
	if !hardfork.SetForBlock(blockNum + 1).Has(hardfork.HF29) {
		t.Fatal("HF29 not active for the block after activation")
	}

	// A second call must not re-apply.
	processHardforks(s, blockNum+1, votedTime+3, nil)
	hpo, _ = objects.GetHardforkPropertyObject(s)
	if hpo.LastHardfork != 29 || len(hpo.ProcessedHardforks) != 30 {
		t.Fatalf("HF29 applied twice: last=%d processed=%d", hpo.LastHardfork, len(hpo.ProcessedHardforks))
	}

	// Restart / fork switch: the table follows the HPO, both ways.
	hardfork.ClearDynamicActivation(hardfork.HF29)
	syncDynamicHardforks(s)
	if !hardfork.SetForBlock(blockNum + 1).Has(hardfork.HF29) {
		t.Fatal("syncDynamicHardforks did not restore the activation from the HPO")
	}
	hpo.DynamicActivations = nil
	hpo.LastHardfork = 28
	objects.PutHardforkPropertyObject(s, &hpo)
	syncDynamicHardforks(s)
	if hardfork.IsScheduled(hardfork.HF29) {
		t.Fatal("syncDynamicHardforks did not clear the activation after the HPO lost it (undo)")
	}
}

// TestHardforkPropertyObjectDynamicActivationRoundTrip pins the trailing
// optional field's encoding: absent when empty (byte-identical to the old
// record), present and decodable otherwise.
func TestHardforkPropertyObjectDynamicActivationRoundTrip(t *testing.T) {
	base := objects.HardforkPropertyObject{
		ProcessedHardforks: []uint32{1, 2, 3}, LastHardfork: 2,
		CurrentHardforkVersion: 7, NextHardfork: 8, NextHardforkTime: 9,
	}
	plain := objects.AppendHardforkPropertyObject(nil, &base)
	withDyn := base
	withDyn.SetDynamicActivation(29, 123456)
	enc := objects.AppendHardforkPropertyObject(nil, &withDyn)
	if len(enc) <= len(plain) {
		t.Fatal("dynamic activation not encoded")
	}
	dec, err := objects.ReadHardforkPropertyObject(enc)
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := dec.DynamicActivationBlock(29); !ok || b != 123456 {
		t.Fatalf("decoded dynamic activation = %d,%v", b, ok)
	}
	old, err := objects.ReadHardforkPropertyObject(plain)
	if err != nil || len(old.DynamicActivations) != 0 {
		t.Fatalf("pre-field record did not decode cleanly: %v %v", err, old.DynamicActivations)
	}
}
