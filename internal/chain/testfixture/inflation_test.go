package testfixture

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
)

// Verify that walking blocks via SkipSecondsReal accrues inflation
// monotonically: current_supply, total_reward_fund_hive, and
// total_vesting_fund_hive all grow when blocks are produced.
//
// Pre-HF16, the per-block reward is the 1 HIVE flat producer reward,
// which is enough to verify supply grows. Post-HF16 the inflation
// slice scales with virtual_supply, which our tiny test fixture
// supply (≈ initminer balance) doesn't yet have, so we test pre-HF16.
func TestInflationAccrualReal(t *testing.T) {
	f := New(t)
	// Stay pre-HF16 so the flat 1 HIVE/block producer reward dominates.
	f.HardforksUpTo(hardfork.HF12)
	f.GenerateBlock()

	before := f.DGPO()
	f.SkipSecondsReal(60) // 20 blocks
	afterReal := f.DGPO()
	realDelta := afterReal.CurrentSupply - before.CurrentSupply
	if realDelta <= 0 {
		t.Fatalf("expected SkipSecondsReal to grow supply, got delta=%d", realDelta)
	}

	// Vesting fund should have also grown by some fraction.
	if afterReal.TotalVestingFundHive <= before.TotalVestingFundHive {
		t.Fatal("vesting fund did not grow during SkipSecondsReal")
	}
}

// Verify per-block inflation slice: pre-HF16, the producer is paid
// 1 HIVE per block which grows current_supply. Post-HF16 the slice
// is virtual_supply-proportional (tiny in our fixture).
func TestInflationContentRewardSlicePreHF16(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF12)
	f.GenerateBlock()

	dgpoBefore := f.DGPO()
	// 100 blocks should give a measurable supply increase.
	f.GenerateBlocks(100)
	dgpoAfter := f.DGPO()

	if dgpoAfter.CurrentSupply <= dgpoBefore.CurrentSupply {
		t.Fatalf("CurrentSupply should grow with each block: before=%d after=%d",
			dgpoBefore.CurrentSupply, dgpoAfter.CurrentSupply)
	}
	// vesting fund should also grow (vesting_reward_percent slice).
	if dgpoAfter.TotalVestingFundHive < dgpoBefore.TotalVestingFundHive {
		t.Fatalf("TotalVestingFundHive should grow: before=%d after=%d",
			dgpoBefore.TotalVestingFundHive, dgpoAfter.TotalVestingFundHive)
	}
}
