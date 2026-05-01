package testfixture

import (
	"testing"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
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
