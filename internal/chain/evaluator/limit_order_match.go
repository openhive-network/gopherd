package evaluator

import (
	"fmt"
	"math/bits"
	"os"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state/objects"
)

// liquidityRewardMinAgePreHF10 — pre-HF10 a maker's order must rest on
// the books at least 1 minute before fills earn liquidity-reward volume
// credit (HIVE_MIN_LIQUIDITY_REWARD_PERIOD_SEC).
const liquidityRewardMinAgePreHF10 uint32 = 60

// liquidityRewardMinAgePostHF10 — HF10 raised the minimum maker age to
// 30 minutes (HIVE_MIN_LIQUIDITY_REWARD_PERIOD_SEC_HF10).
const liquidityRewardMinAgePostHF10 uint32 = 30 * 60

// applyOrder mirrors database::apply_order in
// libraries/chain/database_market.cpp:226. It walks the by_price
// secondary index over orders selling new's *quote* symbol (i.e.
// opposite side), sorted with `composite_key_compare<std::greater<price>,
// std::less<id>>` (best price first, oldest tie-break first), and calls
// match() on each entry whose price still satisfies new's max acceptable
// rate (~new.sell_price). Iteration stops when:
//   - the new order is fully filled (match returns its bit 0 set), OR
//   - the next entry in the index falls below new's acceptable price.
//
// Returns true iff the new order was fully filled (and therefore
// removed from state); false if any portion remained on the book.
func applyOrder(ctx *Context, newOrder *objects.LimitOrder) bool {
	// Iterate the opposite-side by_price index in DESC-price order via
	// the typed LimitOrderStore. The previous K/V Range path captured
	// every candidate's (owner, orderID) up front into a slice — 2.2 GB
	// of cum alloc on a 4M-block run. The typed-store version iterates
	// in-memory btrees with zero per-call alloc and breaks as soon as
	// priceCovers() rejects.
	store := objects.EnsureLimitOrderStore(ctx.DB)
	base := newOrder.MinReceiveSym   // candidate's base = new's quote
	quote := newOrder.ForSaleSymbol  // candidate's quote = new's base
	type candidate struct {
		owner   auth.AccountName
		orderID uint32
	}
	// We must snapshot the candidate list before mutating, because
	// matchOrders may rewrite the by_price btree (partial fill +
	// possible delete-and-reinsert in some edge cases). hived's
	// match loop walks a frozen iterator for the same reason. Stack-
	// growable slice avoids the per-candidate alloc we used to incur
	// from the closure capturing `candidate` on each Range step.
	queue := candidateBuf[:0]
	_ = store.RangeByPrice(base, quote, func(lo *objects.LimitOrder) (bool, error) {
		queue = append(queue, candidate{owner: lo.Owner, orderID: lo.OrderID})
		return true, nil
	})
	candidateBuf = queue

	// max_price = ~new.sell_price. An old order is matchable iff its
	// sell_price >= max_price. We compute that gate via cross-multiply
	// on uint128 (priceCovers).
	for _, c := range queue {
		old, err := objects.GetLimitOrder(ctx.DB, c.owner, c.orderID)
		if err != nil {
			continue // already removed by an earlier match in this iteration
		}
		if old.ForSaleAmount <= 0 {
			continue
		}
		if !priceCovers(&old, newOrder) {
			break // by_price is sorted descending — no further entries qualify
		}
		// match returns 1 (new fully filled), 2 (old fully filled), 3
		// (both fully filled). We continue iterating only when bit-0 is
		// clear (new not fully filled).
		result := matchOrders(ctx, newOrder, &old)
		if result&1 != 0 {
			return true
		}
	}
	return false
}

// candidateBuf is a package-scoped reusable slice for the matching
// candidate list. applyOrder is called serially from a single
// goroutine in the replay/apply path — no need for sync.Pool.
var candidateBuf []struct {
	owner   auth.AccountName
	orderID uint32
}

// priceCovers reports whether old.sell_price >= ~new.sell_price, i.e.
// the candidate's price is still good enough for the new order.
//
// Cross-multiply form: old.sell_price >= ~new.sell_price
//   ⇔ old.base.amount × new.base.amount >= old.quote.amount × new.quote.amount
//
// (Because ~new.sell_price has base/quote swapped, the cross-mul of
// `a >= b` becomes `a.base*b.quote_after_swap >= a.quote*b.base_after_swap`,
// which simplifies to the form above.)
//
// Implementation: native uint128 compare replaces the previous math/big
// pair-alloc — 2 GB cum across a 4M-block run (called once per matching
// candidate). All four inputs are non-negative int64 (asset amounts);
// bits.Mul64 yields the 128-bit product without overflow.
func priceCovers(old, newOrder *objects.LimitOrder) bool {
	aHi, aLo := bits.Mul64(uint64(old.SellPriceBaseAmount), uint64(newOrder.SellPriceBaseAmount))
	bHi, bLo := bits.Mul64(uint64(old.MinReceiveAmt), uint64(newOrder.MinReceiveAmt))
	if aHi != bHi {
		return aHi >= bHi
	}
	return aLo >= bLo
}

// matchOrders mirrors database::match in database_market.cpp:248.
// The match_price is OLD's sell_price (newer order takes the resting
// order's posted price, hived convention).
//
// Returns a bitfield: bit 0 = new fully filled, bit 1 = old fully filled.
func matchOrders(ctx *Context, newOrder, old *objects.LimitOrder) int {
	// Convert old's amount_for_sale into new's base symbol via match_price.
	//   old_for_sale (old.base) × match_price → asset in match_price.quote
	//   match_price.base = old.base, match_price.quote = old.quote = new.base
	// So old_for_sale × match_price = old.ForSaleAmount × old.MinReceiveAmt
	//                                / old.SellPriceBaseAmount in new's base.
	oldForSaleInNewBase := mulDiv(old.ForSaleAmount, old.MinReceiveAmt, old.SellPriceBaseAmount)

	var newOrderPays, newOrderReceives, oldOrderPays, oldOrderReceives int64

	if newOrder.ForSaleAmount <= oldForSaleInNewBase {
		// New order fully fills.
		oldOrderReceives = newOrder.ForSaleAmount
		// new gets new_for_sale × match_price.
		// new.ForSaleAmount is in match_price.quote; result is in
		// match_price.base = old.ForSaleSymbol = new.MinReceiveSym.
		newOrderReceives = mulDiv(newOrder.ForSaleAmount, old.SellPriceBaseAmount, old.MinReceiveAmt)
	} else {
		// Old order fully fills.
		newOrderReceives = old.ForSaleAmount
		oldOrderReceives = oldForSaleInNewBase
	}
	oldOrderPays = newOrderReceives
	newOrderPays = oldOrderReceives

	// Pre-HF12 liquidity-reward volume tracking: if the maker (old)
	// order has rested long enough on the books, credit them volume
	// equal to the HIVE-side leg of this trade and debit the taker by
	// the same amount. Post-HF12 the whole liquidity-reward subsystem
	// is shut off (HF12__178). Mirrors database_market.cpp:294-309.
	if !ctx.Hardforks.Has(hardfork.HF12) {
		minAge := liquidityRewardMinAgePreHF10
		if ctx.Hardforks.Has(hardfork.HF10) {
			minAge = liquidityRewardMinAgePostHF10
		}
		ageOK := old.Created != 0 && ctx.HeadBlockTime >= old.Created+minAge
		if envGhivediffLP != "" && (envGhivediffLP == "*" || old.Owner == envGhivediffLP || newOrder.Owner == envGhivediffLP) {
			fmt.Fprintf(os.Stderr, "GHIVEDIFF LP-MATCH block=%d t=%d old_owner=%s old_id=%d old_created=%d age_ok=%t old_recv_sym=%d old_recv=%d new_recv_sym=%d new_recv=%d new_owner=%s\n",
				ctx.BlockNum, ctx.HeadBlockTime, old.Owner, old.OrderID, old.Created, ageOK,
				old.MinReceiveSym, oldOrderReceives, newOrder.MinReceiveSym, newOrderReceives, newOrder.Owner)
		}
		if ageOK {
			adjustLiquidityRewardOnFill(ctx, old, newOrder, oldOrderReceives, newOrderReceives)
		}
	}

	result := 0
	if fillLimitOrder(ctx, newOrder, newOrderPays, newOrderReceives) {
		result |= 1
	}
	if fillLimitOrder(ctx, old, oldOrderPays, oldOrderReceives) {
		result |= 2
	}
	return result
}

// adjustLiquidityRewardOnFill mirrors the C++ branch in match() (lines
// 295-309): when both legs of a fill are HIVE/HBD, the trade's
// HIVE-side amount is credited to the maker and debited from the taker.
// volumes can go negative for takers; the LP balance row's `weight`
// recomputes with `is_positive` blocking negative-volume rows from
// counting.
func adjustLiquidityRewardOnFill(ctx *Context, old, new *objects.LimitOrder, oldReceives, newReceives int64) {
	// HIVE-side amount of this trade. Either old_receives or new_receives
	// will be in HIVE, depending on which order is selling HIVE.
	var hiveAmount int64
	if asset.AssetNum(old.MinReceiveSym) == asset.HiveSymbol {
		// old receives HIVE; new pays HIVE.
		hiveAmount = oldReceives
	} else if asset.AssetNum(new.MinReceiveSym) == asset.HiveSymbol {
		// new receives HIVE; old pays HIVE.
		hiveAmount = newReceives
	} else {
		// Neither side is HIVE — only HIVE/HBD trades qualify in hived's
		// pre-HF12 LP scheme. Skip.
		return
	}
	if hiveAmount <= 0 {
		return
	}
	// Determine which side is the maker (old) and which symbol they
	// receive. In hived match's branch-1 logic, the volume is always the
	// HIVE-equivalent regardless of leg orientation.
	makerIsHive := asset.AssetNum(old.MinReceiveSym) == asset.HiveSymbol
	hf9 := ctx.Hardforks.Has(hardfork.HF9)
	hf10 := ctx.Hardforks.Has(hardfork.HF10)
	adjustLiquidityVolume(ctx, old.Owner, hiveAmount, hf9, hf10, !makerIsHive)  // maker += hive_amount on its hive_volume side
	adjustLiquidityVolume(ctx, new.Owner, -hiveAmount, hf9, hf10, !makerIsHive) // taker -= same
}

// adjustLiquidityVolume mirrors database::adjust_liquidity_reward
// (database_vesting.cpp:62). Looks up or creates the row; resets
// volumes if the row hasn't been touched in HIVE_LIQUIDITY_TIMEOUT_SEC
// (7d); applies the volume delta; recomputes weight; updates last_update.
//
// hived gates the weight formula on TWO different hardforks depending
// on which path runs:
//   - existing row modify: `update_weight(has_hardfork(HF10))` — only
//     HF10+ flips that branch to MIN; between HF9 and HF10 it keeps
//     PRODUCT.
//   - new row create:      `update_weight(has_hardfork(HF9))` — HF9+
//     uses MIN.
// HF10 then runs `retally_liquidity_weight()` to recompute every
// pre-existing row with the MIN formula. Without this distinction
// our Go fed adm's post-HF9 modifies through MIN immediately, while
// C++ kept feeding them through PRODUCT until HF10. Result: in the
// HF9→HF10 window every account's modify-path weight collapsed to
// MIN here while C++'s stayed at the (much larger) PRODUCT. Stale
// pre-HF9 PRODUCT rows then dominated `pay_liquidity_reward`, so adm
// stopped winning and missed ~9 of its ~172 hourly 1200-HIVE payouts
// (≈10K HIVE), leaving its HIVE balance ~1.7M sat short for the
// limit_order_create that fails at block 3,223,222.
func adjustLiquidityVolume(ctx *Context, owner string, delta int64, hf9, hf10 bool, isHbd bool) {
	const liquidityTimeoutSec uint32 = 60 * 60 * 24 * 7
	now := ctx.HeadBlockTime
	b, err := objects.GetLiquidityRewardBalance(ctx.DB, owner)
	prevWeight := b.Weight
	exists := err == nil
	timedOut := false
	if exists && now-b.LastUpdate >= liquidityTimeoutSec {
		b.HiveVolume = 0
		b.HBDVolume = 0
		timedOut = true
	}
	if !exists {
		b = objects.LiquidityRewardBalance{Owner: owner}
	}
	if isHbd {
		b.HBDVolume += delta
	} else {
		b.HiveVolume += delta
	}
	// existing row: HF10 gate; new row: HF9 gate (matches C++
	// database_vesting.cpp:82 vs 96).
	useMinFormula := hf10
	if !exists {
		useMinFormula = hf9
	}
	b.Weight = objects.ComputeLiquidityWeight(b.HiveVolume, b.HBDVolume, useMinFormula)
	b.LastUpdate = now
	if envGhivediffLP != "" && (owner == envGhivediffLP || envGhivediffLP == "*") {
		fmt.Fprintf(os.Stderr, "GHIVEDIFF LP-VOL block=%d t=%d owner=%s delta=%d isHbd=%t timedOut=%t hive_v=%d hbd_v=%d weight=%x\n",
			ctx.BlockNum, now, owner, delta, isHbd, timedOut, b.HiveVolume, b.HBDVolume, b.Weight)
	}
	if exists {
		objects.PutLiquidityRewardBalanceUpdating(ctx.DB, &b, prevWeight)
	} else {
		objects.PutLiquidityRewardBalance(ctx.DB, &b)
	}
}

var envGhivediffLP = os.Getenv("GHIVEDIFF_LP")

// fillLimitOrder mirrors database::fill_order in database_market.cpp:323.
// Credits the seller their `receives`, decrements `for_sale` by `pays`,
// removes the order if the entire for_sale was consumed (or if the
// remaining amount_to_receive becomes zero — a dust safety valve from
// hived). Returns true when the order was fully consumed.
func fillLimitOrder(ctx *Context, order *objects.LimitOrder, pays, receives int64) bool {
	// GHIVEDIFF_FILLS=1: one line per fill side — diffable against the
	// public API's fill_order vops to bisect order-book divergences.
	if envGhivediffFills != "" {
		ghiveDiff("GHIVEDIFF FILLV block=%d owner=%s orderid=%d pays=%d receives=%d\n",
			ctx.BlockNum, order.Owner, order.OrderID, pays, receives)
	}
	if (ctx.BlockNum >= 2889020 && ctx.BlockNum <= 2891791) || vestTraceAccount(order.Owner) {
		preHive := int64(0)
		preHbd := int64(0)
		if a, gerr := objects.GetAccount(ctx.DB, order.Owner); gerr == nil {
			preHive = a.Balance
			preHbd = a.HBDBalance
		}
		ghiveDiff("GHIVEDIFF FILL block=%d t=%d seller=%s orderid=%d pays=%d pays_sym=%d receives=%d receives_sym=%d for_sale_before=%d pre_hive=%d pre_hbd=%d\n",
			ctx.BlockNum, ctx.HeadBlockTime, order.Owner, order.OrderID,
			pays, order.ForSaleSymbol, receives, order.MinReceiveSym, order.ForSaleAmount,
			preHive, preHbd)
	}
	if pays <= 0 {
		return false
	}
	// Credit the seller `receives` in their MinReceiveSym. C++'s
	// fill_order (database.cpp) ALWAYS calls adjust_balance even when
	// receives rounds to zero — and for HBD that triggers an HBD-
	// seconds accrual (+ possible interest payout) inside
	// modify_balance regardless of delta. Previously we gated the
	// whole branch on `receives > 0`, which skipped the per-account
	// accrual hook for HBD fills that rounded to zero. The drift
	// surfaced at xtar's block 9,642,600 convert (1-sat short of
	// 35238 HBD): C++ had accrued an extra interest sat from an
	// unfilled HBD-receive that our path silently dropped.
	a, err := objects.GetAccount(ctx.DB, order.Owner)
	if err == nil {
		switch asset.AssetNum(order.MinReceiveSym) {
		case asset.HiveSymbol:
			a.Balance += receives
		case asset.HbdSymbol:
			objects.AdjustHBDBalance(ctx.DB, &a, ctx.HeadBlockTime, receives)
		}
		objects.PutAccount(ctx.DB, &a)
	}

	if pays >= order.ForSaleAmount {
		// Order fully consumed.
		objects.DeleteLimitOrderFull(ctx.DB, order)
		return true
	}

	// Partial fill: reduce for_sale and rewrite. Note that we keep
	// SellPriceBaseAmount and MinReceiveAmt at their original values so
	// later matches still see the original price ratio.
	order.ForSaleAmount -= pays
	// Dust safety: if the remaining amount_to_receive
	// (= for_sale × min_to_receive / sell_price.base) rounds to zero,
	// hived cancels the order and refunds the dust.
	if mulDiv(order.ForSaleAmount, order.MinReceiveAmt, order.SellPriceBaseAmount) <= 0 {
		// Refund the residual for_sale to the seller (cancel_order).
		if a, gerr := objects.GetAccount(ctx.DB, order.Owner); gerr == nil {
			switch asset.AssetNum(order.ForSaleSymbol) {
			case asset.HiveSymbol:
				a.Balance += order.ForSaleAmount
			case asset.HbdSymbol:
				objects.AdjustHBDBalance(ctx.DB, &a, ctx.HeadBlockTime, order.ForSaleAmount)
			}
			objects.PutAccount(ctx.DB, &a)
		}
		objects.DeleteLimitOrderFull(ctx.DB, order)
		return true
	}
	objects.PutLimitOrder(ctx.DB, order)
	return false
}

// mulDiv returns a × b / c using uint128 native math to avoid int64
// overflow when the product exceeds 2^63. Result is truncated to int64
// (caller is responsible for checking that the true result fits, which
// is always the case in the matching path: the match formulas are
// bounded by the smaller of the two for-sale amounts). Inputs must be
// non-negative; negative inputs cause undefined results.
func mulDiv(a, b, c int64) int64 {
	if c == 0 {
		return 0
	}
	// Fast path when no overflow can occur.
	if -(1<<31) < a && a < (1<<31) && -(1<<31) < b && b < (1<<31) {
		return a * b / c
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	d := uint64(c)
	if hi >= d {
		// Saturate; this should never trigger for well-formed orders.
		return 1<<63 - 1
	}
	q, _ := bits.Div64(hi, lo, d)
	if q > 1<<63-1 {
		return 1<<63 - 1
	}
	return int64(q)
}
