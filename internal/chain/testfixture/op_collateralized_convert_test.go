package testfixture

import (
	"testing"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// setCollateralizedFeedAndPrintRate injects a price feed (using `p` as
// CurrentMedianHistory, MarketMedianHistory, and CurrentMinHistory),
// zeros out HBDInterestRate so accrued interest doesn't perturb the
// asserted HBD balances, and forces HBDPrintRate=10000 so the HF25
// hbd_print_rate>0 gate passes. Mirrors the C++ test helpers
// set_price_feed() + set_witness_props(hbd_interest_rate=0).
func setCollateralizedFeedAndPrintRate(t *testing.T, f *Fixture, p asset.Price) {
	t.Helper()
	s := state.NewSession(f.State)
	fh, _ := objects.GetFeedHistory(s)
	fh.CurrentMedianHistory = p
	fh.MarketMedianHistory = p
	fh.CurrentMinHistory = p
	objects.PutFeedHistory(s, &fh)
	dgpo, _ := objects.GetDGPO(s)
	dgpo.HBDPrintRate = 10000
	dgpo.HBDInterestRate = 0
	objects.PutDGPO(s, &dgpo)
	if err := s.Commit(false); err != nil {
		t.Fatalf("setCollateralizedFeedAndPrintRate: commit: %v", err)
	}
}

// Port of operation_tests.cpp::collateralized_convert_narrow_price
// (libraries/tests/unit/tests/operation_tests.cpp:3689). Covers a bug
// where applying the 5% fee to a narrow price (1/2) truncated and gave
// the wrong HBD credit.
//
// Setup:
//   price = 1 HBD / 2 HIVE  (i.e. 1 HIVE = 0.5 HBD)
//   alice funded with 42 sat-HIVE (= 0.042 HIVE)
//
// At op-apply:
//   immediate_hive = 42 × 100% / 200%        = 21
//   converted_hbd  = 21 × 1 × 10000 / (2 × 10500) = 10 sat-HBD
//   alice.balance: 42 → 0;  alice.hbd_balance: 0 → 10
//
// At settlement (HIVE_COLLATERALIZED_CONVERSION_DELAY = 3.5 days):
//   required_hive = 10 × 2 × 10500 / (1 × 10000) = 21
//   excess        = 42 - 21 = 21  ← refunded
//   alice.balance: 0 → 21;  alice.hbd_balance unchanged at 10
func TestCollateralizedConvertNarrowPrice(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF25)
	f.Actor("alice")
	f.GenerateBlock()

	// price_1_for_2: 1 HBD = 2 HIVE.
	feed := asset.Price{
		Base:  asset.Asset{Amount: 1, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Quote: asset.Asset{Amount: 2, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
	}
	setCollateralizedFeedAndPrintRate(t, f, feed)

	// "issue_funds( alice, HIVE_asset(42) )"
	f.FundLiquid("alice", 42)
	// One block to update virtual_supply with the freshly-funded HIVE.
	f.GenerateBlock()

	t.Log("--- Test ok - conversion at 50 cents per HIVE, both initial and actual")
	if err := f.ApplyOp(op.CollateralizedConvert{
		Owner: "alice", RequestID: 0, Amount: HIVE(42),
	}); err != nil {
		t.Fatalf("collateralized_convert: %v", err)
	}
	if got := f.Balance("alice"); got != 0 {
		t.Fatalf("alice HIVE after CC: got %d, want 0", got)
	}
	if got := f.HBDBalance("alice"); got != 10 {
		t.Fatalf("alice HBD after CC: got %d, want 10", got)
	}

	// 3.5 days / 3 seconds per block = 100800 blocks. After the CC op
	// the conversion fires at head_block_time_at_apply + delay; we
	// generate up to ONE block before that timestamp and assert no
	// settlement, then one more to trigger.
	delaySecs := uint32(60 * 60 * 24 * 7 / 2)
	advanceToPreSettle(t, f, delaySecs)

	// Mid-window: not yet settled.
	if got := f.Balance("alice"); got != 0 {
		t.Fatalf("alice HIVE pre-settle: got %d, want 0", got)
	}
	if got := f.HBDBalance("alice"); got != 10 {
		t.Fatalf("alice HBD pre-settle: got %d, want 10", got)
	}

	// One more block — settlement fires.
	f.GenerateBlock()
	if got := f.Balance("alice"); got != 21 {
		t.Fatalf("alice HIVE post-settle: got %d, want 21", got)
	}
	if got := f.HBDBalance("alice"); got != 10 {
		t.Fatalf("alice HBD post-settle: got %d, want 10", got)
	}
}

// advanceToPreSettle advances the chain so the next call to
// GenerateBlock lands at exactly conversion_time, where settlement
// fires. The CC op was applied in the block immediately preceding the
// current curBlockTime; the op's conversion field = dgpo.Time when it
// applied + delaySecs = (curBlockTime - 2*BlockInterval) + delaySecs.
// We want the next block's timestamp == that value.
//
// After CC: curBlockTime = T_applyBlock + BlockInterval; the apply
// block's own timestamp was T_apply = T_prev_at_apply + BlockInterval,
// where T_prev_at_apply is what the evaluator saw as HeadBlockTime
// (dgpo.Time when the op fired). So conversion = T_prev_at_apply +
// delaySecs and curBlockTime = T_prev_at_apply + 2*BlockInterval.
// We want next-block-timestamp = T_prev_at_apply + delaySecs - BlockInterval
// (one block BEFORE settlement). Skip-secs = nextTs - curBlockTime =
// delaySecs - 3*BlockInterval.
func advanceToPreSettle(t *testing.T, f *Fixture, delaySecs uint32) {
	t.Helper()
	// SkipSeconds(secs) lands the next block at curBlockTime +
	// (ceil(secs/3) - 1)*3. We want that to equal conversion - 3,
	// i.e. (T₀ + delaySecs - 3), where curBlockTime = T₀ + 6.
	// → ceil(secs/3) - 1 = delaySecs/3 - 3 → ceil(secs/3) = delaySecs/3 - 2
	// → secs in (delaySecs - 9, delaySecs - 6]. Use delaySecs - 6.
	f.SkipSeconds(delaySecs - 2*BlockInterval)
}

// Port of operation_tests.cpp::collateralized_convert_wide_price
// (libraries/tests/unit/tests/operation_tests.cpp:3733). Covers a
// potential overflow when applying the 5% fee to a price whose base
// and quote both approach 2^62.
//
// Setup:
//   base  = 4611686018427387000 HBD
//   quote = 9223372036854774000 HIVE   (≈ base × 2)
//   alice funded with 50_000_000_000 sat-HIVE (= 50M HIVE).
//
// First op: amount = 42M HIVE-precision (42_000_000.000 TESTS =
// 42_000_000_000 sat-HIVE) — must fail with percent_hbd > hbd_stop_percent.
//
// Second op: amount = 4.2M HIVE = 4_200_000_000 sat-HIVE.
//   immediate_hive = 4_200_000_000 × 100% / 200% = 2_100_000_000
//   converted_hbd  ≈ 2_100_000_000 × 1/2 × (100/105) = 1_000_000_000 HBD-sat
//   alice.balance: 50_000_000_000 → 45_800_000_000;  hbd_balance: 0 → 1_000_000_000
//
// At settlement (no median drift):
//   required_hive  ≈ 1_000_000_000 × 2 × (105/100) = 2_100_000_000
//   excess         = 4_200_000_000 - 2_100_000_000 = 2_100_000_000  ← refunded
//   alice.balance final = 45_800_000_000 + 2_100_000_000 = 47_900_000_000
func TestCollateralizedConvertWidePrice(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF25)
	f.Actor("alice")
	f.GenerateBlock()

	// price_1_for_2 with near-2^62 amounts.
	feed := asset.Price{
		Base:  asset.Asset{Amount: 4611686018427387000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Quote: asset.Asset{Amount: 9223372036854774000, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
	}
	setCollateralizedFeedAndPrintRate(t, f, feed)

	f.FundLiquid("alice", 50_000_000_000)
	// One block to update virtual_supply with the freshly-funded HIVE.
	f.GenerateBlock()

	t.Log("--- Test failure on too many HBD in the system")
	// 42_000_000.000 TESTS == 42_000_000_000 sat-HIVE.
	if err := f.ApplyOp(op.CollateralizedConvert{
		Owner: "alice", RequestID: 0, Amount: HIVE(42_000_000_000),
	}); err == nil {
		t.Fatal("expected percent_hbd > hbd_stop_percent failure, got nil")
	}
	// Apply-failure must NOT consume balance.
	if got := f.Balance("alice"); got != 50_000_000_000 {
		t.Fatalf("alice HIVE after failed CC: got %d, want 50000000000", got)
	}

	t.Log("--- Test ok - conversion at 50 cents per HIVE, both initial and actual")
	balanceBefore := f.Balance("alice")
	if err := f.ApplyOp(op.CollateralizedConvert{
		Owner: "alice", RequestID: 1, Amount: HIVE(4_200_000_000),
	}); err != nil {
		t.Fatalf("collateralized_convert: %v", err)
	}
	wantHive := balanceBefore - 4_200_000_000
	if got := f.Balance("alice"); got != wantHive {
		t.Fatalf("alice HIVE after CC: got %d, want %d", got, wantHive)
	}
	if got := f.HBDBalance("alice"); got != 1_000_000_000 {
		t.Fatalf("alice HBD after CC: got %d, want 1_000_000_000", got)
	}

	delaySecs := uint32(60 * 60 * 24 * 7 / 2)
	advanceToPreSettle(t, f, delaySecs)

	if got := f.Balance("alice"); got != wantHive {
		t.Fatalf("alice HIVE pre-settle: got %d, want %d", got, wantHive)
	}
	if got := f.HBDBalance("alice"); got != 1_000_000_000 {
		t.Fatalf("alice HBD pre-settle: got %d, want 1_000_000_000", got)
	}

	f.GenerateBlock()
	wantHive += 2_100_000_000
	if got := f.Balance("alice"); got != wantHive {
		t.Fatalf("alice HIVE post-settle: got %d, want %d", got, wantHive)
	}
	if got := f.HBDBalance("alice"); got != 1_000_000_000 {
		t.Fatalf("alice HBD post-settle: got %d, want 1_000_000_000", got)
	}
}
