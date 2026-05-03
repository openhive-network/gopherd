package testfixture

import (
	"testing"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// TestHBDPriceFeedLimit ports operation_time_tests.cpp::hbd_price_feed_limit.
//
// When the chain's HBD market cap exceeds HIVE_HBD_HARD_LIMIT (10% pre-HF26)
// of the combined HBD+HIVE market cap at the witness-published median price,
// applyHBDHardCap clamps current_median_history UPWARD so HBD's share is
// pinned at the cap. market_median_history holds the un-clamped market
// value; current_min_history reflects the un-clamped low; current_max_history
// is bumped to the clamped value if needed.
//
// Hived's full test posts a comment, votes on it, generates blocks until
// the cashout fills alice's HBD balance, then sets a new exchange rate
// where dgpo.current_hbd_supply is large enough to trip the 10% cap.
// We exercise the same path more directly: seed dgpo with a high HBD
// supply / low HIVE supply ratio, let 7 witnesses publish 1:1, and verify
// the median lands at the clamp price (not the witness-reported median).
func TestHBDPriceFeedLimit(t *testing.T) {
	f := New(t)
	// HBD hard cap is HF14+ (database.cpp:2549).
	f.HardforksUpTo(hardfork.HF14)

	// Stand up 7 witnesses (minFeeds threshold) and publish a uniform
	// 1:1 HBD/HIVE feed.
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

	// Skew supply so HBD market cap > 10% of total at the 1:1 median.
	// hbd_supply / (hbd_supply + current_supply * median) > 10%
	// at median 1:1, hbd_supply / (hbd_supply + current_supply) > 10%
	// → set hbd_supply = 200, current_supply = 1000 → 16.7%.
	{
		s := state.NewSession(f.State)
		dgpo, err := objects.GetDGPO(s)
		if err != nil {
			t.Fatalf("GetDGPO: %v", err)
		}
		dgpo.CurrentHBDSupply = 200
		dgpo.CurrentSupply = 1000
		dgpo.VirtualSupply = dgpo.CurrentSupply + dgpo.CurrentHBDSupply
		objects.PutDGPO(s, &dgpo)
		if err := s.Commit(false); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// Roll forward to ONE block before the feed_history update boundary
	// (every 1200 blocks), then re-skew dgpo right before the boundary,
	// and finally generate the boundary block. Per-block inflation would
	// otherwise drift CurrentSupply between our override and the median
	// recompute.
	startBlock := f.BlockNum
	target := ((startBlock / 1200) + 1) * 1200
	for f.BlockNum < target-1 {
		f.GenerateBlock()
	}
	// Re-pin dgpo at the exact ratio we want.
	{
		s := state.NewSession(f.State)
		dgpo, err := objects.GetDGPO(s)
		if err != nil {
			t.Fatalf("GetDGPO: %v", err)
		}
		dgpo.CurrentHBDSupply = 200
		dgpo.CurrentSupply = 1000
		dgpo.VirtualSupply = dgpo.CurrentSupply + dgpo.CurrentHBDSupply
		objects.PutDGPO(s, &dgpo)
		if err := s.Commit(false); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	f.GenerateBlock()

	fh, err := objects.GetFeedHistory(state.NewSession(f.State))
	if err != nil {
		t.Fatalf("GetFeedHistory: %v", err)
	}

	// Witness-reported median is 1:1 (1000 HBD / 1000 HIVE).
	// HBD hard cap (pre-HF26): min_price = (90 × 200) HBD / (10 × 1000) HIVE
	//                                    = 18000 HBD / 10000 HIVE
	// 18000/10000 = 1.8 > 1.0 → the median gets clamped UP from 1:1 to 18:10.
	wantBase := int64(90 * 200)
	wantQuote := int64(10 * 1000)
	if fh.CurrentMedianHistory.Base.Amount != wantBase ||
		fh.CurrentMedianHistory.Quote.Amount != wantQuote {
		t.Fatalf("CurrentMedianHistory: got %d/%d, want %d/%d (clamped)",
			fh.CurrentMedianHistory.Base.Amount, fh.CurrentMedianHistory.Quote.Amount,
			wantBase, wantQuote)
	}

	// MarketMedianHistory is the un-clamped market median = 1000/1000.
	if fh.MarketMedianHistory.Base.Amount != 1000 || fh.MarketMedianHistory.Quote.Amount != 1000 {
		t.Fatalf("MarketMedianHistory: got %d/%d, want 1000/1000 (un-clamped)",
			fh.MarketMedianHistory.Base.Amount, fh.MarketMedianHistory.Quote.Amount)
	}

	// CurrentMinHistory is the un-clamped low (= 1000/1000 since all 7 feeds
	// are identical).
	if fh.CurrentMinHistory.Base.Amount != 1000 {
		t.Fatalf("CurrentMinHistory.Base: got %d, want 1000",
			fh.CurrentMinHistory.Base.Amount)
	}

	// CurrentMaxHistory is bumped to the clamp price (the high tracks the
	// clamp ceiling per applyHBDHardCap line: if priceLess(max, minPrice) {
	// max = minPrice }).
	if fh.CurrentMaxHistory.Base.Amount != wantBase ||
		fh.CurrentMaxHistory.Quote.Amount != wantQuote {
		t.Fatalf("CurrentMaxHistory: got %d/%d, want %d/%d (clamped to min_price)",
			fh.CurrentMaxHistory.Base.Amount, fh.CurrentMaxHistory.Quote.Amount,
			wantBase, wantQuote)
	}
}

// TestHBDPriceFeedNoLimitWhenSupplyShareUnderCap verifies that the median
// is left alone when HBD supply is below the 10% threshold.
func TestHBDPriceFeedNoLimitWhenSupplyShareUnderCap(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF14)

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

	// Set HBD share at 5% (under the 10% cap).
	{
		s := state.NewSession(f.State)
		dgpo, err := objects.GetDGPO(s)
		if err != nil {
			t.Fatalf("GetDGPO: %v", err)
		}
		dgpo.CurrentHBDSupply = 50
		dgpo.CurrentSupply = 1000
		dgpo.VirtualSupply = dgpo.CurrentSupply + dgpo.CurrentHBDSupply
		objects.PutDGPO(s, &dgpo)
		if err := s.Commit(false); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	startBlock := f.BlockNum
	target := ((startBlock / 1200) + 1) * 1200
	for f.BlockNum < target {
		f.GenerateBlock()
	}

	fh, err := objects.GetFeedHistory(state.NewSession(f.State))
	if err != nil {
		t.Fatalf("GetFeedHistory: %v", err)
	}

	// Median stays at the witness-reported 1:1.
	if fh.CurrentMedianHistory.Base.Amount != 1000 || fh.CurrentMedianHistory.Quote.Amount != 1000 {
		t.Fatalf("CurrentMedianHistory: got %d/%d, want 1000/1000 (no clamp)",
			fh.CurrentMedianHistory.Base.Amount, fh.CurrentMedianHistory.Quote.Amount)
	}
}
