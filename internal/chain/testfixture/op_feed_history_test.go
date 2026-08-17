package testfixture

import (
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// Verify that CurrentMedianHistory updates after enough blocks pass
// (HIVE_FEED_INTERVAL_BLOCKS = 1200 blocks ≈ 1 hour). With 3
// witnesses publishing 900/1000/1100, the median should land at 1000.
func TestFeedHistoryMedianUpdate(t *testing.T) {
	f := New(t)

	// Need at least minFeeds (7) active witnesses for the median to
	// update. We create 7 witnesses publishing 700/800/900/1000/1100/
	// 1200/1300 — median = 1000.
	names := []string{"w1", "w2", "w3", "w4", "w5", "w6", "w7"}
	for _, name := range names {
		f.Actor(name)
		f.FundLiquid(name, 10_000)
		if err := f.ApplyOp(op.WitnessUpdate{
			Owner: name, URL: "x",
			BlockSigningKey: [33]byte{0x02, 1, byte(name[1])},
			Props: op.LegacyChainProperties{
				AccountCreationFee: LegacyHIVE(1_000),
				MaximumBlockSize:   65536, HBDInterestRate: 1000,
			},
		}); err != nil {
			t.Fatalf("witness %s: %v", name, err)
		}
	}

	prices := []int64{700, 800, 900, 1000, 1100, 1200, 1300}
	for i, name := range names {
		feed := asset.Price{
			Base:  asset.Asset{Amount: prices[i], Symbol: asset.Symbol{Num: asset.HbdSymbol}},
			Quote: asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
		}
		if err := f.ApplyOp(op.FeedPublish{
			Publisher: name, ExchangeRate: feed,
		}); err != nil {
			t.Fatalf("feed_publish %s: %v", name, err)
		}
	}

	// updateMedianFeed runs every 1200 blocks (one feed interval) and
	// only when blockNum % feedIntervalBlocks == 0. Generate enough
	// blocks to land on a multiple of 1200. SkipSeconds fast-path
	// produces a single block at the target, which is sufficient for
	// the median computation since it only depends on the on-chain
	// witness feeds.
	startBlock := f.BlockNum
	target := ((startBlock / 1200) + 1) * 1200
	for f.BlockNum < target {
		f.GenerateBlock()
	}

	fh, err := objects.GetFeedHistory(state.NewSession(f.State))
	if err != nil {
		t.Fatalf("GetFeedHistory: %v", err)
	}
	if fh.CurrentMedianHistory.Base.Amount == 0 {
		t.Fatal("CurrentMedianHistory should be set after a feed interval")
	}
	if fh.CurrentMedianHistory.Base.Amount != 1000 {
		t.Fatalf("CurrentMedianHistory.Base.Amount: got %d, want 1000",
			fh.CurrentMedianHistory.Base.Amount)
	}
}

// TestFeedPublishMeanRotates ports the rolling-window slice of
// operation_time_tests.cpp::feed_publish_mean. After the first
// hour-interval feed update, the price_history vector has 1 entry; one
// more feed-interval later it has 2; the current median follows the
// MIDDLE of that window. Mirrors hived's update_median_feed
// (database.cpp:3322) — every HIVE_FEED_INTERVAL_BLOCKS we (a) sample
// each witness's last-published rate, append the median of *that* sample
// to the price_history, and (b) recompute current_median_history as the
// median of the price_history vector itself.
func TestFeedPublishMeanRotates(t *testing.T) {
	f := New(t)

	names := []string{"a0", "a1", "a2", "a3", "a4", "a5", "a6"}
	for _, n := range names {
		f.Actor(n)
		f.FundLiquid(n, 10_000)
		if err := f.ApplyOp(op.WitnessUpdate{
			Owner: n, URL: "x",
			BlockSigningKey: [33]byte{0x02, 1, byte(n[1])},
			Props: op.LegacyChainProperties{
				AccountCreationFee: LegacyHIVE(1_000),
				MaximumBlockSize:   65536, HBDInterestRate: 1000,
			},
		}); err != nil {
			t.Fatalf("witness %s: %v", n, err)
		}
	}

	// Publish initial 7 feeds: 100000 / 105000 / 98000 / 97000 / 99000 /
	// 97500 / 102000 (quote). The median quote across all 7 = 99000 (a4).
	// Mirrors the C++ source numbers.
	quotes := []int64{100000, 105000, 98000, 97000, 99000, 97500, 102000}
	for i, n := range names {
		feed := asset.Price{
			Base:  asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
			Quote: asset.Asset{Amount: quotes[i], Symbol: asset.Symbol{Num: asset.HiveSymbol}},
		}
		if err := f.ApplyOp(op.FeedPublish{Publisher: n, ExchangeRate: feed}); err != nil {
			t.Fatalf("feed %s: %v", n, err)
		}
	}

	// Generate enough blocks to land on the next feed-interval boundary
	// (HIVE_FEED_INTERVAL_BLOCKS = 1200, ~1h).
	advanceToFeedInterval := func() {
		target := ((f.BlockNum / 1200) + 1) * 1200
		for f.BlockNum < target {
			f.GenerateBlock()
		}
	}
	advanceToFeedInterval()

	fh, err := objects.GetFeedHistory(state.NewSession(f.State))
	if err != nil {
		t.Fatalf("GetFeedHistory: %v", err)
	}
	if got := fh.CurrentMedianHistory.Quote.Amount; got != 99000 {
		t.Fatalf("median quote after 1st interval: got %d, want 99000", got)
	}
	if len(fh.PriceHistory) != 1 {
		t.Fatalf("price_history len after 1st interval: got %d, want 1", len(fh.PriceHistory))
	}

	// Bump every witness's quote by +10 and roll another hour. The new
	// per-window median = 99010; the current_median is the median of the
	// 2-entry history {99000, 99010} → 99000 (lower of two).
	for i, n := range names {
		feed := asset.Price{
			Base:  asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
			Quote: asset.Asset{Amount: quotes[i] + 10, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
		}
		if err := f.ApplyOp(op.FeedPublish{Publisher: n, ExchangeRate: feed}); err != nil {
			t.Fatalf("feed bump %s: %v", n, err)
		}
	}
	advanceToFeedInterval()
	fh, _ = objects.GetFeedHistory(state.NewSession(f.State))
	if len(fh.PriceHistory) != 2 {
		t.Fatalf("price_history len after 2nd interval: got %d, want 2", len(fh.PriceHistory))
	}
	if got := fh.PriceHistory[1].Quote.Amount; got != 99010 {
		t.Fatalf("price_history[1] quote: got %d, want 99010", got)
	}
}
