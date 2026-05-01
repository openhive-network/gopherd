package evaluator

import (
	"math/big"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
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
	// Snapshot opposite-side candidates from the by_price index.
	// hived's by_price is sorted DESCENDING — encoded into our key as
	// bit-flipped uint128 — so iterating the prefix [base=quoteSym,
	// quote=baseSym] forward already gives us best-price-first.
	prefix := state.AppendUint32BE(nil, newOrder.MinReceiveSym) // candidate's base = new's quote
	prefix = state.AppendUint32BE(prefix, newOrder.ForSaleSymbol) // candidate's quote = new's base

	type candidate struct {
		owner   string
		orderID uint32
	}
	var queue []candidate
	_ = ctx.DB.Range(objects.LimitOrderTable, objects.LimitOrderByPriceIndex, prefix,
		func(body, _ []byte) (bool, error) {
			// body layout:
			//   [base_sym:4][quote_sym:4][price_inv:16][owner:16][orderid:4]
			const off = 4 + 4 + 16
			if len(body) < off+state.AccountNameKeyLen+4 {
				return true, nil
			}
			ownerBytes := body[off : off+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && ownerBytes[n-1] == 0 {
				n--
			}
			oid := uint32(body[off+state.AccountNameKeyLen])<<24 |
				uint32(body[off+state.AccountNameKeyLen+1])<<16 |
				uint32(body[off+state.AccountNameKeyLen+2])<<8 |
				uint32(body[off+state.AccountNameKeyLen+3])
			queue = append(queue, candidate{owner: string(ownerBytes[:n]), orderID: oid})
			return true, nil
		})

	// max_price = ~new.sell_price = (newMinReceive, quote_sym) /
	//                              (newForSaleOriginal, base_sym).
	// An old order is matchable iff its sell_price >= max_price. We
	// compute that gate via cross-multiply on uint128.
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

// priceCovers reports whether old.sell_price >= ~new.sell_price, i.e.
// the candidate's price is still good enough for the new order.
//
// Cross-multiply form: old.sell_price >= ~new.sell_price
//   ⇔ old.base.amount × new.base.amount >= old.quote.amount × new.quote.amount
//
// (Because ~new.sell_price has base/quote swapped, the cross-mul of
// `a >= b` becomes `a.base*b.quote_after_swap >= a.quote*b.base_after_swap`,
// which simplifies to the form above.)
func priceCovers(old, newOrder *objects.LimitOrder) bool {
	a := mulBig(old.SellPriceBaseAmount, newOrder.SellPriceBaseAmount)
	b := mulBig(old.MinReceiveAmt, newOrder.MinReceiveAmt)
	return a.Cmp(b) >= 0
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
		if old.Created != 0 && ctx.HeadBlockTime >= old.Created+minAge {
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
	adjustLiquidityVolume(ctx, old.Owner, hiveAmount, !makerIsHive, hf9)  // maker += hive_amount on its hive_volume side
	adjustLiquidityVolume(ctx, new.Owner, -hiveAmount, !makerIsHive, hf9) // taker -= same
}

// adjustLiquidityVolume mirrors database::adjust_liquidity_reward
// (database_vesting.cpp:62). Looks up or creates the row; resets
// volumes if the row hasn't been touched in HIVE_LIQUIDITY_TIMEOUT_SEC
// (7d); applies the volume delta; recomputes weight; updates last_update.
func adjustLiquidityVolume(ctx *Context, owner string, delta int64, isHbd bool, hf9 bool) {
	const liquidityTimeoutSec uint32 = 60 * 60 * 24 * 7
	now := ctx.HeadBlockTime
	b, err := objects.GetLiquidityRewardBalance(ctx.DB, owner)
	prevWeight := b.Weight
	exists := err == nil
	if exists && now-b.LastUpdate >= liquidityTimeoutSec {
		b.HiveVolume = 0
		b.HBDVolume = 0
	}
	if !exists {
		b = objects.LiquidityRewardBalance{Owner: owner}
	}
	if isHbd {
		b.HBDVolume += delta
	} else {
		b.HiveVolume += delta
	}
	b.Weight = objects.ComputeLiquidityWeight(b.HiveVolume, b.HBDVolume, hf9)
	b.LastUpdate = now
	if exists {
		objects.PutLiquidityRewardBalanceUpdating(ctx.DB, &b, prevWeight)
	} else {
		objects.PutLiquidityRewardBalance(ctx.DB, &b)
	}
}

// fillLimitOrder mirrors database::fill_order in database_market.cpp:323.
// Credits the seller their `receives`, decrements `for_sale` by `pays`,
// removes the order if the entire for_sale was consumed (or if the
// remaining amount_to_receive becomes zero — a dust safety valve from
// hived). Returns true when the order was fully consumed.
func fillLimitOrder(ctx *Context, order *objects.LimitOrder, pays, receives int64) bool {
	if ghiveDiffOwnerMatch(order.Owner) || ghiveDiffWide(ctx.BlockNum) {
		ghiveDiff("GHIVEDIFF FILL block=%d t=%d seller=%s orderid=%d pays=%d pays_sym=%d receives=%d receives_sym=%d for_sale_before=%d\n",
			ctx.BlockNum, ctx.HeadBlockTime, order.Owner, order.OrderID,
			pays, order.ForSaleSymbol, receives, order.MinReceiveSym, order.ForSaleAmount)
	}
	if pays <= 0 || receives <= 0 {
		return false
	}
	// Credit the seller `receives` in their MinReceiveSym.
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

// mulDiv returns a × b / c using math/big to avoid int64 overflow when
// the product exceeds 2^63. Result is truncated to int64 (caller is
// responsible for checking that the true result fits, which is always
// the case in the matching path: the match formulas are bounded by
// the smaller of the two for-sale amounts).
func mulDiv(a, b, c int64) int64 {
	if c == 0 {
		return 0
	}
	// Fast path when no overflow can occur.
	if -(1<<31) < a && a < (1<<31) && -(1<<31) < b && b < (1<<31) {
		return a * b / c
	}
	prod := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	prod.Quo(prod, big.NewInt(c))
	if !prod.IsInt64() {
		// Saturate; this should never trigger for well-formed orders.
		if prod.Sign() > 0 {
			return 1<<63 - 1
		}
		return -(1 << 62)
	}
	return prod.Int64()
}

// mulBig returns a × b as a math/big.Int — used by priceCovers when
// comparing two prices via cross-multiplication without precision loss.
func mulBig(a, b int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
}
