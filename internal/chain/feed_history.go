package chain

import (
	"fmt"
	"math/bits"
	"os"
	"sort"
	"strings"

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
		// position-by-position from 0 to wso.num_scheduled_witnesses
		// (NOT the full slice length — older slice entries from a
		// previous round may linger past the new num_scheduled count
		// after `ws.CurrentShuffledWitnesses = shuffled` if the
		// underlying slice's capacity is reused). Match C++ by capping
		// the iteration at NumScheduledWitnesses.
		count := int(ws.NumScheduledWitnesses)
		if count > len(ws.CurrentShuffledWitnesses) {
			count = len(ws.CurrentShuffledWitnesses)
		}
		for i := 0; i < count; i++ {
			n := ws.CurrentShuffledWitnesses[i]
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
				// hived (database.cpp:2413): include iff now < last+AGE, i.e.
				// EXCLUDE when now >= last+AGE. Go used `>` which includes a
				// feed exactly at the 7-day boundary that hived excludes.
				// Match hived with `>=`.
				if w.LastHBDExchangeUpdate == 0 ||
					blockTime >= w.LastHBDExchangeUpdate+maxFeedAgeSec {
					continue
				}
			}
			feeds = append(feeds, w.HBDExchangeRate)
		}
	}

	if envGhivediffFeed != "" && vsDumpFrom != 0 && uint64(blockNum) >= vsDumpFrom && uint64(blockNum) <= vsDumpTo {
		var sched []string
		if ws, err := objects.GetWitnessSchedule(s); err == nil {
			for _, n := range ws.CurrentShuffledWitnesses {
				name := string(n)
				if name != "" {
					sched = append(sched, name)
				}
			}
		}
		sort.Strings(sched)
		fmt.Fprintf(os.Stderr, "GHIVEDIFF FEED-SCHED block=%d feeds=%d schedule=[%s]\n",
			blockNum, len(feeds), strings.Join(sched, ","))
		// Also dump top-25 witnesses by votes to compare voted-set composition.
		// Uses the typed WitnessStore — the legacy K/V WitnessTable ranges went
		// permanently empty after the typed-store overhaul, which silently
		// emptied these dumps (feedp.log 2026-07-01 had top25=[]).
		wstore := objects.EnsureWitnessStore(s)
		var entries []string
		_ = wstore.RangeByVote(func(w *objects.Witness) (bool, error) {
			entries = append(entries, fmt.Sprintf("%s=%d", w.Owner, w.TotalVotesVests))
			return len(entries) < 25, nil
		})
		fmt.Fprintf(os.Stderr, "GHIVEDIFF SCHED-VOTES block=%d top25=[%s]\n",
			blockNum, strings.Join(entries, ","))
		// Top-30 by smallest VST (timeshare candidates), with position +
		// last_update + votes for cross-side diff.
		var vstEntries, posEntries []string
		_ = wstore.RangeByScheduleTime(func(w *objects.Witness) (bool, error) {
			vstEntries = append(vstEntries, fmt.Sprintf("%s=%x", w.Owner, w.VirtualScheduledTime))
			posEntries = append(posEntries, fmt.Sprintf("%s:pos=%x,lu=%x,votes=%d",
				w.Owner, w.VirtualPosition, w.VirtualLastUpdate, w.TotalVotesVests))
			return len(vstEntries) < 30, nil
		})
		fmt.Fprintf(os.Stderr, "GHIVEDIFF SCHED-VSTS block=%d top30=[%s]\n",
			blockNum, strings.Join(vstEntries, ","))
		fmt.Fprintf(os.Stderr, "GHIVEDIFF SCHED-POS block=%d top30=[%s]\n",
			blockNum, strings.Join(posEntries, ","))
	}

	if len(feeds) < minFeeds {
		return
	}

	sort.Slice(feeds, func(i, j int) bool { return priceLess(feeds[i], feeds[j]) })
	medianFeed := feeds[len(feeds)/2]
	if envGhivediffFeed != "" && vsDumpFrom != 0 && uint64(blockNum) >= vsDumpFrom && uint64(blockNum) <= vsDumpTo {
		fmt.Fprintf(os.Stderr, "GHIVEDIFF FEED-MEDIAN block=%d median_base=%d median_quote=%d\n",
			blockNum, medianFeed.Base.Amount, medianFeed.Quote.Amount)
	}

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
		if envGhivediffFeed != "" && vsDumpFrom != 0 && uint64(blockNum) >= vsDumpFrom && uint64(blockNum) <= vsDumpTo {
			fmt.Fprintf(os.Stderr, "GHIVEDIFF FEED-HIST block=%d hist_len=%d cmh_base=%d cmh_quote=%d\n",
				blockNum, len(fh.PriceHistory),
				copyHist[len(copyHist)/2].Base.Amount,
				copyHist[len(copyHist)/2].Quote.Amount)
		}
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
		// HIVE_HARDFORK_1_25_HBD_HARD_CAP (= HF25; database.cpp:2522-2523):
		// treasury HBD is held outside the open market, so it is EXCLUDED
		// from the hard-cap's hbd_supply. Treasury is hive.fund post-HF24
		// (HF25 > HF24), steem.dao otherwise — resolve via the hardfork-
		// aware helper.
		if t, terr := objects.GetAccount(s, currentTreasuryName(s, hf)); terr == nil {
			hbdSupply -= t.HBDBalance
		}
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

// dumpFullWitnessTSV writes ALL witnesses' state (block, name, votes,
// position, last_update, scheduled_time, schedule) as TSV to a file.
// One run captures everything we need for cross-side per-witness diff —
// no block-list gating, no recompile loops.
//
// Format: tab-separated, one row per witness per block. A trailing
// `__ws__` row carries ws.current_virtual_time so consumers can recover
// the chain-wide virtual time at each rebuild.
//
// File is opened in append mode and written for every call. The chain
// caller is single-threaded, so there's no locking overhead.
var fullDumpFp *os.File
var fullDumpPath string

func dumpFullWitnessTSV(s *state.Session, blockNum uint32, ws *objects.WitnessSchedule, path string) {
	if fullDumpFp == nil || fullDumpPath != path {
		if fullDumpFp != nil {
			fullDumpFp.Close()
		}
		f, err := os.Create(path)
		if err != nil {
			return
		}
		fullDumpFp = f
		fullDumpPath = path
		fmt.Fprintln(fullDumpFp, "block\tname\tvotes\tposition\tlast_update\tscheduled_time\tschedule\tpow_worker")
	}
	var names []string
	_ = objects.EnsureWitnessStore(s).RangePrimary(func(w *objects.Witness) (bool, error) {
		names = append(names, string(w.Owner))
		return true, nil
	})
	sort.Strings(names) // deterministic order, matches hived by_name iter
	for _, name := range names {
		w, err := objects.GetWitness(s, auth.AccountName(name))
		if err != nil {
			continue
		}
		// Skip witnesses with VST == uint128_max AND votes == 0 — they're
		// at the END of by_schedule_time and never visited in rebuild
		// iter. Mirrors hived's GHIVEDIFF_DUMP filter exactly so cross-
		// side TSV diff aligns row-for-row.
		isMaxVST := true
		for _, b := range w.VirtualScheduledTime {
			if b != 0xff {
				isMaxVST = false
				break
			}
		}
		if isMaxVST && w.TotalVotesVests == 0 {
			continue
		}
		// Remap Go's WitnessScheduleType (None=0, Elected=1, Timeshare=2,
		// Miner=3) to C++'s enum order (elected=0, timeshare=1, miner=2,
		// none=3) so the diff tool sees equal values for equal states.
		schedCpp := 3
		switch w.Schedule {
		case objects.WitnessScheduleElected:
			schedCpp = 0
		case objects.WitnessScheduleTimeshare:
			schedCpp = 1
		case objects.WitnessScheduleMiner:
			schedCpp = 2
		}
		fmt.Fprintf(fullDumpFp, "%d\t%s\t%d\t%x\t%x\t%x\t%d\t%d\n",
			blockNum, name, w.TotalVotesVests,
			w.VirtualPosition, w.VirtualLastUpdate, w.VirtualScheduledTime,
			schedCpp, w.PowWorker)
	}
	// __ws__ row carrying ws.current_virtual_time + dgpo num_pow_witnesses
	dgpo, _ := objects.GetDGPO(s)
	fmt.Fprintf(fullDumpFp, "%d\t__ws__\t0\t%032x\t%032x\t%x\t0\t%d\n",
		blockNum, []byte{}, []byte{}, ws.CurrentVirtualTime, dgpo.NumPowWitnesses)
}

// dumpSchedVSTs prints SCHED-VSTS and SCHED-POS for the top-30 witnesses
// by smallest VST. Used at checkpoint blocks for cross-side parity diff.
func dumpSchedVSTs(s *state.Session, blockNum uint32) {
	var vstEntries []string
	_ = s.Range(objects.WitnessTable, objects.WitnessByScheduleTimeIndex, nil,
		func(key, _ []byte) (bool, error) {
			if len(vstEntries) >= 30 || len(key) < 16+state.AccountNameKeyLen {
				return len(vstEntries) < 30, nil
			}
			vstHex := fmt.Sprintf("%x", key[:16])
			ownerBytes := key[16 : 16+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && ownerBytes[n-1] == 0 {
				n--
			}
			name := string(ownerBytes[:n])
			vstEntries = append(vstEntries, fmt.Sprintf("%s=%s", name, vstHex))
			return true, nil
		})
	fmt.Fprintf(os.Stderr, "GHIVEDIFF SCHED-VSTS block=%d top30=[%s]\n",
		blockNum, strings.Join(vstEntries, ","))
	var posEntries []string
	_ = s.Range(objects.WitnessTable, objects.WitnessByScheduleTimeIndex, nil,
		func(key, _ []byte) (bool, error) {
			if len(posEntries) >= 30 || len(key) < 16+state.AccountNameKeyLen {
				return len(posEntries) < 30, nil
			}
			ownerBytes := key[16 : 16+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && ownerBytes[n-1] == 0 {
				n--
			}
			name := string(ownerBytes[:n])
			w, _ := objects.GetWitness(s, auth.AccountName(name))
			posEntries = append(posEntries, fmt.Sprintf("%s:pos=%x,lu=%x,votes=%d",
				name, w.VirtualPosition, w.VirtualLastUpdate, w.TotalVotesVests))
			return true, nil
		})
	fmt.Fprintf(os.Stderr, "GHIVEDIFF SCHED-POS block=%d top30=[%s]\n",
		blockNum, strings.Join(posEntries, ","))
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
	// a < b  ⇔  a.base × b.quote  <  b.base × a.quote. Use uint128 native
	// math/bits — replaces the math/big triple-alloc that fired on every
	// median-feed sort (call site fires for every witness's published
	// feed on every 100-block median update, ~260 MB cum / 4M blocks).
	aHi, aLo := bits.Mul64(uint64(a.Base.Amount), uint64(b.Quote.Amount))
	bHi, bLo := bits.Mul64(uint64(b.Base.Amount), uint64(a.Quote.Amount))
	if aHi != bHi {
		return aHi < bHi
	}
	return aLo < bLo
}
