package chain

import (
	"math/big"
	"sort"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
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

	// Iterate only the witnesses in the current schedule (top-21 active
	// set), mirroring C++ database.cpp:2476-2494 which iterates
	// `wso.current_shuffled_witnesses` rather than the full witness
	// table. Including non-scheduled witnesses inflates the input set
	// and drifts the median (e.g. block 2,894,400 — Go computed median
	// 235/1000 over 39 feeds, C++ computed it over the 21 scheduled
	// only, yielding a different rolling-window result).
	var feeds []asset.Price
	if ws, err := objects.GetWitnessSchedule(s); err == nil && ws.NumScheduledWitnesses > 0 {
		seen := make(map[string]bool)
		for i := uint8(0); i < ws.NumScheduledWitnesses && int(i) < len(ws.CurrentShuffledWitnesses); i++ {
			name := string(ws.CurrentShuffledWitnesses[i])
			if seen[name] {
				continue
			}
			seen[name] = true
			w, werr := objects.GetWitness(s, auth.AccountName(name))
			if werr != nil {
				continue
			}
			if w.LastHBDExchangeUpdate == 0 ||
				blockTime > w.LastHBDExchangeUpdate+maxFeedAgeSec {
				continue
			}
			if priceIsZero(w.HBDExchangeRate) {
				continue
			}
			feeds = append(feeds, w.HBDExchangeRate)
		}
	}

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

// priceLess compares two prices via cross-multiply. Mirrors C++'s
// DEFINE_PRICE_COMPARISON_OPERATOR (libraries/protocol/asset.cpp:291)
// which casts to uint128 unconditionally — float fallback is wrong
// because float rounding is non-deterministic across platforms and
// silently desyncs the median feed at chain head.
//
//	a < b  ⇔  a.base × b.quote  <  b.base × a.quote
func priceLess(a, b asset.Price) bool {
	// If symbol pairs differ, hived (via std::tie on (base.sym, quote.sym))
	// orders by the symbol pair lexicographically. We approximate with
	// the base symbol number — only matters when feeds with mixed
	// orientations land in the same sort, which we don't expect at
	// steady state but which the witness-feed gather above can produce.
	if a.Base.Symbol.Num != b.Base.Symbol.Num {
		return a.Base.Symbol.Num < b.Base.Symbol.Num
	}
	amult := new(big.Int).Mul(big.NewInt(a.Base.Amount), big.NewInt(b.Quote.Amount))
	bmult := new(big.Int).Mul(big.NewInt(b.Base.Amount), big.NewInt(a.Quote.Amount))
	return amult.Cmp(bmult) < 0
}
