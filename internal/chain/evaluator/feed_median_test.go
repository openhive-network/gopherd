package evaluator

import (
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/state/objects"
)

func hbdHive(hbd, hive int64) asset.Price {
	return asset.Price{
		Base:  asset.Asset{Amount: hbd, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Quote: asset.Asset{Amount: hive, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
	}
}

// hived 67a927815: in control, a feed more than 100× from the market median
// is refused; at exactly 100× it passes; in a block it is never checked.
func TestCheckFeedNearMedian(t *testing.T) {
	old := maxFeedDeviationFromMedian
	maxFeedDeviationFromMedian = 100
	defer func() { maxFeedDeviationFromMedian = old }()

	s := seedProposals(t, nil, nil) // any fresh session
	objects.PutFeedHistory(s, &objects.FeedHistory{MarketMedianHistory: hbdHive(1000, 4000)})
	ctx := &Context{DB: s, InControl: true}
	for _, tc := range []struct {
		feed asset.Price
		ok   bool
	}{
		{hbdHive(1000, 4000), true},
		{hbdHive(1000, 400000), true},  // exactly 100× lower
		{hbdHive(1000, 400001), false}, // just past it
		{hbdHive(100000, 4000), true},  // exactly 100× higher
		{hbdHive(100001, 4000), false},
	} {
		if err := checkFeedNearMedian(ctx, tc.feed); (err == nil) != tc.ok {
			t.Errorf("feed %d/%d: err=%v, want ok=%v", tc.feed.Base.Amount, tc.feed.Quote.Amount, err, tc.ok)
		}
	}
	ctx.InControl = false
	if err := checkFeedNearMedian(ctx, hbdHive(1, 1_000_000_000)); err != nil {
		t.Fatalf("checked in a block: %v", err)
	}
}
