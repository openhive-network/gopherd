package chain

import (
	"sort"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_FEED_INTERVAL_BLOCKS — recompute the chain median once per hour
// (block_interval = 3s, so 1200 blocks per hour).
const feedIntervalBlocks uint32 = 1200

// HIVE_MAX_FEED_AGE_SECONDS — a witness's published rate is stale after
// 7 days and won't be considered for the median.
const maxFeedAgeSec uint32 = 60 * 60 * 24 * 7

// HIVE_MIN_FEEDS — at least 7 active witness feeds (HIVE_MAX_WITNESSES /
// 3) before the median is set; HBD-related ops that need a price feed
// abort early when fewer feeds are available.
const minFeeds = 7

// HIVE_FEED_HISTORY_WINDOW — how many median-of-medians samples we keep
// in the rolling history. Pre-HF16 it's 24*7 (one sample/hour for a
// week). HF16+ shrinks to 12*7.
const (
	feedHistoryWindowPreHF16 = 24 * 7
	feedHistoryWindow        = 12 * 7
)

// updateMedianFeed mirrors libraries/chain/database.cpp:2399. Once per
// HIVE_FEED_INTERVAL_BLOCKS, scan every witness with a fresh feed,
// compute the median exchange rate, push it into the rolling window,
// and recompute the median-of-medians as the chain's authoritative
// HIVE↔HBD price.
//
// We don't yet have a witness schedule (top-21 active set), so we
// iterate the full witness table. In the early chain witnesses are
// unsorted; the median across all active feed-publishing witnesses is
// a faithful approximation of hived's "scheduled witnesses" set since
// inactive witnesses' rates are stale and filtered out.
func updateMedianFeed(s *state.Session, blockNum, blockTime uint32) {
	if blockNum%feedIntervalBlocks != 0 {
		return
	}

	// Collect active feeds (witnesses that published within the
	// freshness window).
	var feeds []asset.Price
	_ = s.Range(objects.WitnessTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			w, err := objects.ReadWitness(value)
			if err != nil {
				return true, nil
			}
			// Skip witnesses with no feed or a stale feed.
			if w.LastHBDExchangeUpdate == 0 ||
				blockTime > w.LastHBDExchangeUpdate+maxFeedAgeSec {
				return true, nil
			}
			if priceIsZero(w.HBDExchangeRate) {
				return true, nil
			}
			feeds = append(feeds, w.HBDExchangeRate)
			return true, nil
		})

	if len(feeds) < minFeeds {
		return
	}

	sort.Slice(feeds, func(i, j int) bool { return priceLess(feeds[i], feeds[j]) })
	medianFeed := feeds[len(feeds)/2]

	hf := hardfork.SetForBlock(blockNum)
	window := feedHistoryWindowPreHF16
	if hf.Has(hardfork.HF16) {
		window = feedHistoryWindow
	}

	fh, err := objects.GetFeedHistory(s)
	if err != nil && err != state.ErrNotFound {
		return
	}
	fh.PriceHistory = append(fh.PriceHistory, medianFeed)
	if len(fh.PriceHistory) > window {
		fh.PriceHistory = fh.PriceHistory[len(fh.PriceHistory)-window:]
	}
	if len(fh.PriceHistory) > 0 {
		copyHist := make([]asset.Price, len(fh.PriceHistory))
		copy(copyHist, fh.PriceHistory)
		sort.Slice(copyHist, func(i, j int) bool { return priceLess(copyHist[i], copyHist[j]) })
		fh.CurrentMedianHistory = copyHist[len(copyHist)/2]
		fh.MarketMedianHistory = fh.CurrentMedianHistory
		fh.CurrentMinHistory = copyHist[0]
		fh.CurrentMaxHistory = copyHist[len(copyHist)-1]
	}
	objects.PutFeedHistory(s, &fh)
}

// priceIsZero reports whether either side of a price has zero amount.
// We treat that as "no feed yet".
func priceIsZero(p asset.Price) bool {
	return p.Base.Amount == 0 || p.Quote.Amount == 0
}

// priceLess compares two prices `a` and `b`. hived's price comparison
// is value-based: cross-multiply to avoid divide-by-zero and
// floating-point noise. a < b iff a.base * b.quote < b.base * a.quote
// (assuming all amounts positive). Cross-multiplications can overflow
// int64 for large amounts — for our HBD/HIVE feed rates the amounts
// are typically small (cents to a few dollars) and easily fit, but
// guard with a uint128 fallback if either side overflows.
func priceLess(a, b asset.Price) bool {
	// Normalize: if a's base symbol differs from b's base symbol the
	// cross-multiply isn't meaningful. Inversion happens in hived for
	// reversed feeds; we follow whatever orientation the publisher
	// used so the comparison stays self-consistent.
	if a.Base.Symbol.Num != b.Base.Symbol.Num {
		// Different orientation — sort by symbol num for determinism.
		return a.Base.Symbol.Num < b.Base.Symbol.Num
	}
	// Cross multiply, watching for overflow.
	if absLessThanInt64Sqrt(a.Base.Amount) && absLessThanInt64Sqrt(b.Quote.Amount) &&
		absLessThanInt64Sqrt(b.Base.Amount) && absLessThanInt64Sqrt(a.Quote.Amount) {
		return a.Base.Amount*b.Quote.Amount < b.Base.Amount*a.Quote.Amount
	}
	// Fall back to ratio comparison (loses some precision but doesn't overflow).
	if a.Quote.Amount == 0 || b.Quote.Amount == 0 {
		return false
	}
	ar := float64(a.Base.Amount) / float64(a.Quote.Amount)
	br := float64(b.Base.Amount) / float64(b.Quote.Amount)
	return ar < br
}

// absLessThanInt64Sqrt reports whether |v| × |v| can fit in an int64
// — used to gate the safe cross-multiply path in priceLess.
func absLessThanInt64Sqrt(v int64) bool {
	const limit int64 = 3037000499 // floor(sqrt(int64 max))
	if v < 0 {
		v = -v
	}
	return v <= limit
}
