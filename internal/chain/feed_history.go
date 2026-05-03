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

	// Iterate the witnesses that produced the most recent
	// HIVE_MAX_WITNESSES (21) blocks. C++ uses
	// `wso.current_shuffled_witnesses` — the schedule rebuilt every
	// round from max_voted (19 by-vote) + max_miner (1 from PoW queue)
	// + timeshare (virtual-time fairness queue). We don't model the
	// miner queue or virtual-time, so the proper top-by-vote subset
	// only fills 19/20 entries; the 21st slot is consistently
	// mis-picked, which drifts the rolling-window median feed (e.g.
	// block 2,922,000 — Go median 228/1000 vs C++ median 230/1000) and
	// starves later HBD payouts (fatjawjones limit_order_create at
	// block 2,924,280, etc.).
	//
	// Recover the right SET by reading whoever actually produced the
	// last 21 blocks — that's C++'s active schedule by definition,
	// since the chain ran on C++'s schedule and the headers we replay
	// carry C++'s producer names verbatim.
	hf := hardfork.SetForBlock(blockNum)
	var feeds []asset.Price
	if ws, err := objects.GetWitnessSchedule(s); err == nil {
		// C++ database.cpp:2476-2494 iterates wso.current_shuffled_witnesses
		// position-by-position. The full-parity update_witness_schedule_4
		// fills CurrentShuffledWitnesses with 21 entries (max_voted +
		// max_miner + max_runner timeshare via virtual_scheduled_time)
		// matching C++'s active set.
		for _, n := range ws.CurrentShuffledWitnesses {
			name := string(n)
			if name == "" {
				continue
			}
			w, werr := objects.GetWitness(s, auth.AccountName(name))
			if werr != nil {
				continue
			}
			if priceIsZero(w.HBDExchangeRate) {
				continue
			}
			if hf.Has(hardfork.HF19) {
				if w.LastHBDExchangeUpdate == 0 ||
					blockTime > w.LastHBDExchangeUpdate+maxFeedAgeSec {
					continue
				}
			}
			feeds = append(feeds, w.HBDExchangeRate)
		}
	}

	if len(feeds) < minFeeds {
		return
	}

	sort.Slice(feeds, func(i, j int) bool { return priceLess(feeds[i], feeds[j]) })
	medianFeed := feeds[len(feeds)/2]

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

		// HF14+ HBD hard-cap clamp on current_median_history. Mirrors
		// libraries/chain/database.cpp:2549-2602. If the chain's HBD
		// market cap exceeds HIVE_HBD_HARD_LIMIT (10% pre-HF26, 30%
		// HF26+) of the combined HBD+HIVE market cap at the median
		// price, clamp the median UP to the price that would cap HBD
		// at the limit. Prevents an HBD whale from converting to
		// HIVE during a price crash.
		//
		// Formula:
		//   limit_pct = 100 (HIVE_100_PERCENT/HIVE_1_PERCENT)
		//   limit     = 10 (PRE-HF26) or 30 (HF26+)
		//   min_price = ((100 - limit) × hbd_supply) HBD
		//             / ( limit × current_supply ) HIVE
		//   if min_price > current_median_history → clamp up.
		//
		// HF25+ subtracts treasury HBD from hbd_supply (treasury's
		// HBD doesn't sit on user balances, so it shouldn't be
		// counted toward the hard-cap calculation).
		if hf.Has(hardfork.HF14) {
			applyHBDHardCap(s, &fh, hf)
		}
	}
	objects.PutFeedHistory(s, &fh)
}

// applyHBDHardCap clamps fh.CurrentMedianHistory upward when the chain's
// HBD supply share exceeds the hard-cap limit at the current median.
// Mirrors database.cpp:2549-2602. Also clamps CurrentMaxHistory if
// min_price exceeds it; never modifies MarketMedianHistory.
func applyHBDHardCap(s *state.Session, fh *objects.FeedHistory, hf hardfork.Set) {
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return
	}
	hbdSupply := dgpo.CurrentHBDSupply
	if hf.Has(hardfork.HF25) {
		// Subtract treasury balance (treasury's HBD is held outside
		// the open market). Mirrors the HIVE_HARDFORK_1_25_HBD_HARD_CAP
		// gate at database.cpp:2573. We don't yet model the treasury
		// account explicitly here, so the subtraction is a no-op
		// pre-HF25 and we'll wire it when HF25 lands.
		_ = hbdSupply
	}
	if hbdSupply <= 0 {
		return
	}
	// HIVE_HBD_HARD_LIMIT_PRE_HF26 = 1000 (10%)
	// HIVE_HBD_HARD_LIMIT          = 3000 (30%)  — HF26+
	// HIVE_1_PERCENT = 100, HIVE_100_PERCENT = 10000
	// So limit_pct (in /HIVE_1_PERCENT units) = HIVE_HBD_HARD_LIMIT / 100.
	limit := int64(10) // 10% pre-HF26 (= HIVE_HBD_HARD_LIMIT_PRE_HF26 / HIVE_1_PERCENT)
	if hf.Has(hardfork.HF26) {
		limit = 30
	}
	// min_price.base  = (100 - limit) × hbd_supply  in HBD-sat units
	// min_price.quote = limit         × current_supply in HIVE-sat units
	minBaseAmt := (100 - limit) * hbdSupply
	minQuoteAmt := limit * dgpo.CurrentSupply
	if minQuoteAmt == 0 {
		return
	}
	minPrice := asset.Price{
		Base:  asset.Asset{Amount: minBaseAmt, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Quote: asset.Asset{Amount: minQuoteAmt, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
	}
	if priceLess(fh.CurrentMedianHistory, minPrice) {
		fh.CurrentMedianHistory = minPrice
	}
	if priceLess(fh.CurrentMaxHistory, minPrice) {
		fh.CurrentMaxHistory = minPrice
	}
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
