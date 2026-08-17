package testfixture

import (
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// SetPriceFeed pokes the feed_history singleton directly with `p` so
// convert / collateralized_convert ops pass the
// validate_price_feed_available gate. Mirrors C++
// database_fixture::set_price_feed — direct state poke, no witness
// quorum required (saves dozens of lines of witness scaffolding in
// each test).
//
// Sets all three price fields (CurrentMedianHistory, MarketMedianHistory,
// CurrentMinHistory) to the same value, matching the C++ helper. Does
// NOT touch DGPO — tests that need print-rate / HBD interest tweaks
// should poke those separately.
func (f *Fixture) SetPriceFeed(t *testing.T, p asset.Price) {
	t.Helper()
	s := state.NewSession(f.State)
	fh, _ := objects.GetFeedHistory(s)
	fh.CurrentMedianHistory = p
	fh.MarketMedianHistory = p
	fh.CurrentMinHistory = p
	objects.PutFeedHistory(s, &fh)
	if err := s.Commit(false); err != nil {
		t.Fatalf("SetPriceFeed: commit: %v", err)
	}
}

// DefaultPriceFeed returns the 1:1 HBD/HIVE price every C++
// set_price_feed( HBD_price(1000,1000) ) call site uses by default.
func DefaultPriceFeed() asset.Price {
	return asset.Price{
		Base:  asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Quote: asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
	}
}

// witnessUpdateOp returns a minimal witness_update operation for the
// named account with a deterministic block-signing key based on the
// account name. Used by tests that need a registered witness without
// caring about the specific signing key.
func witnessUpdateOp(owner string) op.WitnessUpdate {
	keySeed := byte(0)
	if len(owner) > 0 {
		keySeed = owner[0]
	}
	return op.WitnessUpdate{
		Owner:           owner,
		URL:             "x",
		BlockSigningKey: [33]byte{0x02, 1, keySeed},
		Props: op.LegacyChainProperties{
			AccountCreationFee: LegacyHIVE(1_000),
			MaximumBlockSize:   65536,
			HBDInterestRate:    1000,
		},
		Fee: HIVE(0),
	}
}

// feedPublishOp returns a feed_publish operation for the named publisher
// and exchange rate.
func feedPublishOp(publisher string, rate asset.Price) op.FeedPublish {
	return op.FeedPublish{
		Publisher:    publisher,
		ExchangeRate: rate,
	}
}
