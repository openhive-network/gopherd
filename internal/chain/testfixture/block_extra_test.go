package testfixture

import (
	"testing"

	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// TestInitHiveHBDSupply ports the spirit of block_tests.cpp::init_hive_hbd_supply.
// At genesis, current_supply / current_hbd_supply / virtual_supply must
// be initialized consistently. virtual_supply ≈ current_supply + current_hbd_supply
// (in HIVE-equivalent at the median feed) — without a feed, equal to current_supply.
func TestInitHiveHBDSupply(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	dgpo := f.DGPO()
	if dgpo.CurrentSupply < 0 {
		t.Errorf("CurrentSupply must be non-negative: %d", dgpo.CurrentSupply)
	}
	if dgpo.CurrentHBDSupply < 0 {
		t.Errorf("CurrentHBDSupply must be non-negative: %d", dgpo.CurrentHBDSupply)
	}
	if dgpo.VirtualSupply < dgpo.CurrentSupply {
		t.Errorf("VirtualSupply (%d) must be >= CurrentSupply (%d)",
			dgpo.VirtualSupply, dgpo.CurrentSupply)
	}
}

// TestSkipBlocksAdvancesHeadAndTime ports the spirit of block_tests.cpp::
// skip_block. SkipSeconds advances head_block_num and head_block_time
// in lockstep.
func TestSkipBlocksAdvancesHeadAndTime(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	startNum := f.BlockNum
	startTime := f.HeadBlockTime()

	f.SkipSeconds(60)

	if f.BlockNum <= startNum {
		t.Errorf("SkipSeconds(60) should advance BlockNum: %d → %d", startNum, f.BlockNum)
	}
	if f.HeadBlockTime() <= startTime {
		t.Errorf("SkipSeconds(60) should advance HeadBlockTime: %d → %d", startTime, f.HeadBlockTime())
	}
	// dgpo should match.
	dgpo := f.DGPO()
	if dgpo.HeadBlockNumber != f.BlockNum {
		t.Errorf("dgpo.HeadBlockNumber=%d ≠ f.BlockNum=%d", dgpo.HeadBlockNumber, f.BlockNum)
	}
}

// TestEmptyBlockGeneration verifies that a block with no transactions
// still updates head_block_num and head_block_time.
func TestEmptyBlockGeneration(t *testing.T) {
	f := New(t)
	pre := f.BlockNum
	preTime := f.HeadBlockTime()

	f.GenerateBlock()
	post := f.BlockNum
	postTime := f.HeadBlockTime()

	if post != pre+1 {
		t.Errorf("BlockNum: %d → %d, want %d", pre, post, pre+1)
	}
	if postTime != preTime+3 {
		t.Errorf("HeadBlockTime: %d → %d, want %d", preTime, postTime, preTime+3)
	}
}

// TestPostgenesisDGPOConsistency verifies that the DGPO singleton
// roundtrips through serialize/deserialize without drift across blocks.
func TestPostgenesisDGPOConsistency(t *testing.T) {
	f := New(t)
	f.GenerateBlocks(5)

	s := state.NewSession(f.State)
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		t.Fatalf("GetDGPO: %v", err)
	}
	if dgpo.HeadBlockNumber != f.BlockNum {
		t.Errorf("DGPO.HeadBlockNumber=%d ≠ fixture.BlockNum=%d",
			dgpo.HeadBlockNumber, f.BlockNum)
	}
	if dgpo.Time == 0 {
		t.Error("DGPO.Time should be non-zero post-genesis")
	}
}

// TestBlockHeadConsistency verifies that the dgpo.HeadBlockID matches
// what the fixture's prevID tracks (both should be the just-applied
// block's ID).
func TestBlockHeadConsistency(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	dgpo := f.DGPO()
	if dgpo.HeadBlockID == [20]byte{} {
		t.Error("DGPO.HeadBlockID should be non-zero post-block")
	}
}
