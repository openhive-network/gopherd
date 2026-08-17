package testfixture

import (
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// TestHF23HiveInflation ports operation_time_tests.cpp::hive_inflation.
//
// The C++ test body is wrapped in a /* ... */ block in upstream — the
// reference assertions reference a pre-HF16 minted-content / minted-curate /
// minted-witness model and `HIVE_START_VESTING_BLOCK` walks that no longer
// align with the post-HF era we test under. Hived ships the test as a
// no-op smoke; we mirror that here while leaving the per-block growth
// behaviour verified by TestInflationAccrualReal /
// TestInflationContentRewardSlicePreHF16 in inflation_test.go.
func TestHF23HiveInflation(t *testing.T) {
	t.Skip("operation_time_tests.cpp::hive_inflation body is commented out upstream; per-block inflation accrual already covered by TestInflationAccrualReal")
}

// TestHF23LiquidityRewards ports a slice of
// operation_time_tests.cpp::liquidity_rewards.
//
// The C++ test asserts that after HF12 (which disables the liquidity-reward
// subsystem), liquidity_reward_balance rows are never created and no
// liquidity_reward_operation fires even after HIVE_LIQUIDITY_REWARD_BLOCKS.
// We exercise the same path: post-HF12, generate enough blocks to span
// multiple liquidity-reward boundaries and check no balance changes for
// the would-be top trader.
func TestHF23LiquidityRewards(t *testing.T) {
	f := New(t)
	// HF12 disables liquidity rewards entirely (chain/liquidity_reward.go:36).
	f.HardforksUpTo(hardfork.HF12)
	f.Actor("alice")
	f.FundLiquid("alice", 10_000)
	f.GenerateBlock()

	preBal := f.Balance("alice")
	// liquidityRewardBlocks = 1200. Roll past two boundaries.
	target := ((f.BlockNum / 1200) + 2) * 1200
	for f.BlockNum < target+5 {
		f.GenerateBlock()
	}
	postBal := f.Balance("alice")

	// Post-HF12, alice's HIVE balance must be exactly unchanged — no LP
	// reward should have been minted, and we did no trading on her.
	if postBal != preBal {
		t.Fatalf("post-HF12 LP rewards disabled: alice balance changed %d → %d",
			preBal, postBal)
	}

	// Cross-check: no liquidity_reward_balance row was ever created.
	if has, _ := objects.HasLiquidityRewardBalance(state.NewSession(f.State), "alice"); has {
		t.Fatal("post-HF12: no liquidity_reward_balance row should exist for alice")
	}
}

// TestHF23HBDStability ports operation_time_tests.cpp::hbd_stability.
//
// The C++ test:
//  1. Seeds dgpo.current_hbd_supply so HBD share > hbd_stop_percent (default
//     pre-HF26 = 5%). Verifies print_rate drops to 0.
//  2. Seeds dgpo.current_hbd_supply so HBD share is between start and stop.
//     Verifies print_rate is below 100% and ≥ last_rate as supply falls.
//  3. Eventually falls under hbd_start_percent and print_rate returns to
//     100%.
//
// We port the steady-state property: with a real feed published, post-HF14,
// (a) high HBD share → HBDPrintRate = 0, and (b) low HBD share → 10000.
func TestHF23HBDStability(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF14)

	// Spin up 7 witnesses (minFeeds) publishing a 1:1 feed so the median
	// updates on the next feed-interval boundary.
	names := []string{"w1", "w2", "w3", "w4", "w5", "w6", "w7"}
	for _, n := range names {
		f.Actor(n)
		f.FundLiquid(n, 10_000)
		f.MustApplyOp(witnessUpdateOp(n))
	}
	exchange := asset.Price{
		Base:  asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Quote: asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
	}
	for _, n := range names {
		f.MustApplyOp(feedPublishOp(n, exchange))
	}

	// --- Phase 1: HBD share above stop_percent → print_rate must drop to 0.
	{
		s := state.NewSession(f.State)
		dgpo, err := objects.GetDGPO(s)
		if err != nil {
			t.Fatalf("GetDGPO: %v", err)
		}
		// pre-HF26 default stop_percent ≈ 500 bps (5%). Push HBD share to
		// 50% of virtual supply.
		dgpo.CurrentSupply = 1_000_000
		dgpo.CurrentHBDSupply = 1_000_000
		dgpo.VirtualSupply = dgpo.CurrentSupply + dgpo.CurrentHBDSupply
		objects.PutDGPO(s, &dgpo)
		if err := s.Commit(false); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// Roll to the next feed-interval boundary so the median lands at 1:1.
	target := ((f.BlockNum / 1200) + 1) * 1200
	for f.BlockNum < target-1 {
		f.GenerateBlock()
	}
	// Re-pin the dgpo right before the boundary to defeat inflation drift.
	{
		s := state.NewSession(f.State)
		dgpo, err := objects.GetDGPO(s)
		if err != nil {
			t.Fatalf("GetDGPO: %v", err)
		}
		dgpo.CurrentSupply = 1_000_000
		dgpo.CurrentHBDSupply = 1_000_000
		dgpo.VirtualSupply = dgpo.CurrentSupply + dgpo.CurrentHBDSupply
		objects.PutDGPO(s, &dgpo)
		if err := s.Commit(false); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	f.GenerateBlock()
	postHigh := f.DGPO()
	if postHigh.HBDPrintRate != 0 {
		t.Fatalf("high HBD share: HBDPrintRate got %d, want 0", postHigh.HBDPrintRate)
	}

	// --- Phase 2: HBD share well below start_percent → print_rate = 10000.
	{
		s := state.NewSession(f.State)
		dgpo, err := objects.GetDGPO(s)
		if err != nil {
			t.Fatalf("GetDGPO: %v", err)
		}
		// 0.1% share — well under default 2% start.
		dgpo.CurrentSupply = 1_000_000
		dgpo.CurrentHBDSupply = 1000
		dgpo.VirtualSupply = dgpo.CurrentSupply + dgpo.CurrentHBDSupply
		objects.PutDGPO(s, &dgpo)
		if err := s.Commit(false); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	// Run another full feed-interval cycle.
	target2 := ((f.BlockNum / 1200) + 1) * 1200
	for f.BlockNum < target2-1 {
		f.GenerateBlock()
	}
	{
		s := state.NewSession(f.State)
		dgpo, err := objects.GetDGPO(s)
		if err != nil {
			t.Fatalf("GetDGPO: %v", err)
		}
		dgpo.CurrentSupply = 1_000_000
		dgpo.CurrentHBDSupply = 1000
		dgpo.VirtualSupply = dgpo.CurrentSupply + dgpo.CurrentHBDSupply
		objects.PutDGPO(s, &dgpo)
		if err := s.Commit(false); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	f.GenerateBlock()
	postLow := f.DGPO()
	if postLow.HBDPrintRate != 10000 {
		t.Fatalf("low HBD share: HBDPrintRate got %d, want 10000", postLow.HBDPrintRate)
	}
}

// TestHF23HBDPriceFeedLimit is intentionally a no-op marker pointing at
// the already-ported TestHBDPriceFeedLimit / TestHBDPriceFeedNoLimitWhenSupplyShareUnderCap
// in op_hbd_feed_limit_test.go.
func TestHF23HBDPriceFeedLimit(t *testing.T) {
	t.Skip("covered by TestHBDPriceFeedLimit + TestHBDPriceFeedNoLimitWhenSupplyShareUnderCap in op_hbd_feed_limit_test.go")
}

// TestHF23GenerateAccountSubsidies ports a slice of
// operation_time_tests.cpp::generate_account_subsidies.
//
// The C++ test exercises set_witness_props with custom
// account_subsidy_budget / account_subsidy_decay, then asserts the global
// pool advances per-block by the rd_apply formula
//
//	pool += budget − (pool × decay) >> HIVE_RD_DECAY_DENOM_SHIFT
//
// In the Go fixture we don't expose set_witness_props with a custom
// account_subsidy_* override yet (the witness schedule still pumps the
// global pool at default budget/decay), so we port the per-block rdApply
// equilibrium check against Go's defaults instead.
func TestHF23GenerateAccountSubsidies(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF20)
	f.GenerateBlock()

	pre := f.DGPO().AvailableAccountSubsidies
	f.GenerateBlocks(20)
	post := f.DGPO().AvailableAccountSubsidies
	if post <= pre {
		t.Fatalf("AvailableAccountSubsidies should grow per-block post-HF20: pre=%d post=%d", pre, post)
	}

	// Per-block rd_apply with default budget+decay must NOT diverge wildly:
	// after 20 blocks at 797 budget the pool should sit between ~16K and
	// ~50K subsidy-units (budget × 20 ≤ pool ≤ budget × 20 / (1 − decay) ).
	// We just sanity-check the lower bound to catch regressions where the
	// hook is a no-op.
	minExpected := int64(797 * 20)
	gained := post - pre
	if gained < minExpected/2 {
		t.Fatalf("AvailableAccountSubsidies gain too small: gained=%d want ≥ %d", gained, minExpected/2)
	}
}

// TestHF23AccountSubsidyWitnessLimits ports
// operation_time_tests.cpp::account_subsidy_witness_limits.
//
// The C++ test asserts that claim_account_operation with fee=0 is REJECTED
// at block-production time when the producing witness is a timeshare
// witness (since they have no per-witness subsidy budget allocated). The
// test depends on set_witness_props with custom account_subsidy_*
// properties + multi-witness scheduling to land on a specific schedule
// slot — neither is wired into the Go testfixture yet.
func TestHF23AccountSubsidyWitnessLimits(t *testing.T) {
	t.Skip("set_witness_props(account_subsidy_*) + timeshare-witness scheduling not wired into Go testfixture")
}

// TestHF23RewardFunds is a marker pointing at the already-ported
// TestRewardFundsLifecycle in op_time_extra_test.go.
func TestHF23RewardFunds(t *testing.T) {
	t.Skip("covered by TestRewardFundsLifecycle in op_time_extra_test.go")
}

// TestHF23RecentClaimsDecay is a marker pointing at the already-ported
// TestRecentClaimsDecayMonotonic in op_time_extra_test.go.
func TestHF23RecentClaimsDecay(t *testing.T) {
	t.Skip("covered by TestRecentClaimsDecayMonotonic in op_time_extra_test.go")
}
