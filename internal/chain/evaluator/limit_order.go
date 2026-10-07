package evaluator

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// HIVE_HARDFORK_0_20_TIME (libraries/protocol/hardfork.d/0_20.hf:22).
// Pre-HF20 limit_order expirations are capped at HF20_TIME + 28d (+ a
// per-block random offset for create, no offset for create2) so that any
// excessively-long expirations from the pre-HF20 era can't survive past
// the HF20 cutover. Post-HF20, the op-level assert
// `o.expiration <= now + 28d` enforces the same window inline.
const hf20TimeSec uint32 = 1537887600

// HIVE_MAX_LIMIT_ORDER_EXPIRATION = 28 days. Both the pre-HF20 cap and
// the post-HF20 op assert use this window.
const maxLimitOrderExpirationSec uint32 = 28 * 24 * 60 * 60

// cappedExpirationCreate mirrors the pre-HF20 branch of
// hive_evaluator_transfer.cpp:537-538 — the expiration on a pre-HF20
// limit_order_create is capped at `HF20_TIME + 28d + rand_offset`, where
// rand_offset = head_block_id._hash[4] % 86400. Post-HF20, returns the
// raw expiration (the op-level assert at do_apply line 532 enforces the
// 28-day window).
//
// `head_block_id` here is the id of the most recently applied block —
// which during in-tx evaluation is the block before the one being applied,
// matching what hived's head_block_id() returns at this point.
func cappedExpirationCreate(ctx *Context, expiration uint32) uint32 {
	if ctx.Hardforks.Has(hardfork.HF20) {
		return expiration
	}
	dgpo, err := objects.GetDGPO(ctx.DB)
	if err != nil {
		return expiration
	}
	// fc::ripemd160 stores _hash as uint32[5]; on x86 little-endian
	// _hash[4] is bytes 16..19 read as a little-endian uint32 (the same
	// byte ordering hived uses to compute the modulo).
	randOffset := binary.LittleEndian.Uint32(dgpo.HeadBlockID[16:20]) % 86400
	cap := hf20TimeSec + maxLimitOrderExpirationSec + randOffset
	if expiration > cap {
		return cap
	}
	return expiration
}

// cappedExpirationCreate2 is the create2 counterpart (hive_evaluator_transfer.cpp:583)
// — same cap, but WITHOUT the rand_offset. The asymmetry between the two
// evaluators is preserved on mainnet because both ops shipped this way
// pre-HF20.
func cappedExpirationCreate2(ctx *Context, expiration uint32) uint32 {
	if ctx.Hardforks.Has(hardfork.HF20) {
		return expiration
	}
	cap := hf20TimeSec + maxLimitOrderExpirationSec
	if expiration > cap {
		return cap
	}
	return expiration
}

// ghiveDiff emits a GHIVEDIFF-tagged stderr line whenever the GHIVEDIFF
// env var is set, matching the format of the patched hived in
// libraries/chain/database_market.cpp + hive_evaluator_transfer.cpp. Used
// to side-by-side diff replay traces. Filtered to adm/abit by default.
// Uses the cached package-level envGhivediff to avoid the per-call
// os.Getenv syscall on the cashout-cascade hot path (CASHOUT-CURATOR /
// CASHOUT-AUTHOR traces fire from every CreateVesting in cashout).
func ghiveDiff(format string, args ...any) {
	if envGhivediff == "" {
		return
	}
	fmt.Fprintf(os.Stderr, format, args...)
}

func ghiveDiffOwnerMatch(owner string) bool {
	return owner == "adm" || owner == "abit" || owner == "jsc"
}

// ghiveDiffWide returns true when the diff trace should match the C++
// "wide" log window (every owner) — currently the 20-block neighborhood
// of the adm/20001 cancel divergence at block 2,889,559.
func ghiveDiffWide(blockNum uint32) bool {
	return blockNum >= 2738850 && blockNum <= 2890900
}

// Errors specific to limit_order evaluators.
var (
	ErrLimitOrderExists     = errors.New("limit order with this orderid already exists")
	ErrLimitOrderNotFound   = errors.New("limit order not found")
	ErrLimitOrderExpired    = errors.New("limit order expiration has passed")
	ErrLimitOrderSameSymbol = errors.New("limit order amount_to_sell and min_to_receive must differ in symbol")
)

// limitOrderCreateEvaluator implements limit_order_create.
//
// Mirrors libraries/chain/hive_evaluator_transfer.cpp:522:
//   - validate expiration is in the future (28-day cap post-HF20)
//   - debit owner.balance by amount_to_sell
//   - create limit_order_object
//   - apply_order: try to match against existing opposite-side orders
//     in the by_price index; partial fills update for_sale, full fills
//     remove the order
//   - if fill_or_kill and the new order didn't fully fill, the op fails
type limitOrderCreateEvaluator struct{}

func (limitOrderCreateEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagLimitOrderCreate {
		return fmt.Errorf("limitOrderCreateEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	lo := opArg.AsLimitOrderCreate()
	if ghiveDiffOwnerMatch(lo.Owner) || ghiveDiffWide(ctx.BlockNum) {
		ghiveDiff("GHIVEDIFF CREATE block=%d t=%d owner=%s orderid=%d sell=%d/%d recv=%d/%d exp=%d\n",
			ctx.BlockNum, ctx.HeadBlockTime, lo.Owner, lo.OrderID,
			lo.AmountToSell.Amount, lo.AmountToSell.Symbol.Num,
			lo.MinToReceive.Amount, lo.MinToReceive.Symbol.Num,
			lo.Expiration)
	}
	// Mirrors C++ `_db.create<limit_order_object>(o.owner, o.amount_to_sell,
	// o.get_price(), ...)` where get_price() = price(amount_to_sell,
	// min_to_receive). The base/quote of sell_price are exactly the
	// amount_to_sell.amount / min_to_receive.amount pair.
	sellPrice := asset.Price{Base: lo.AmountToSell, Quote: lo.MinToReceive}
	err := applyLimitOrderCreate(ctx, lo.Owner, lo.OrderID, lo.AmountToSell, sellPrice,
		cappedExpirationCreate(ctx, lo.Expiration), lo.FillOrKill)
	if err != nil && (ghiveDiffOwnerMatch(lo.Owner) || ghiveDiffWide(ctx.BlockNum)) {
		ghiveDiff("GHIVEDIFF CREATE-FAIL block=%d t=%d owner=%s orderid=%d err=%v\n",
			ctx.BlockNum, ctx.HeadBlockTime, lo.Owner, lo.OrderID, err)
	}
	return err
}

type limitOrderCreate2Evaluator struct{}

func (limitOrderCreate2Evaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagLimitOrderCreate2 {
		return fmt.Errorf("limitOrderCreate2Evaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	lo := opArg.AsLimitOrderCreate2()
	if ghiveDiffOwnerMatch(lo.Owner) || ghiveDiffWide(ctx.BlockNum) {
		ghiveDiff("GHIVEDIFF CREATE2 block=%d t=%d owner=%s orderid=%d sell=%d/%d rate=%d/%d/%d/%d exp=%d\n",
			ctx.BlockNum, ctx.HeadBlockTime, lo.Owner, lo.OrderID,
			lo.AmountToSell.Amount, lo.AmountToSell.Symbol.Num,
			lo.ExchangeRate.Base.Amount, lo.ExchangeRate.Base.Symbol.Num,
			lo.ExchangeRate.Quote.Amount, lo.ExchangeRate.Quote.Symbol.Num,
			lo.Expiration)
	}
	if lo.ExchangeRate.Base.Amount <= 0 || lo.ExchangeRate.Quote.Amount <= 0 {
		return fmt.Errorf("%w: invalid exchange_rate", ErrTransferAmountNotPos)
	}
	// C++ stores `sell_price = o.exchange_rate` verbatim
	// (hive_evaluator_transfer.cpp:587). amount_to_sell goes into
	// for_sale; the price ratio that drives by_price ordering and the
	// per-fill receive math is exchange_rate.base/quote — NOT the derived
	// min_to_receive. Earlier we collapsed to a derived min_to_receive,
	// which gave a 7:4-vs-3:2 divergence whenever
	// amount_to_sell.amount != exchange_rate.base.amount.
	return applyLimitOrderCreate(ctx, lo.Owner, lo.OrderID, lo.AmountToSell, lo.ExchangeRate,
		cappedExpirationCreate2(ctx, lo.Expiration), lo.FillOrKill)
}

// applyLimitOrderCreate is the shared back-half of both limit_order_create
// and limit_order_create2 evaluators. It takes the order's amount_to_sell
// (which is debited and put on the book as for_sale) and its sell_price
// (sell_price.base + sell_price.quote) — kept separate so create2 can
// preserve a sell_price ratio that doesn't equal amount_to_sell, matching
// C++'s `_db.create<limit_order_object>(seller, amount_to_sell, sell_price,
// ...)` constructor. The base/quote pair drives the by_price index entry
// and all subsequent matching math.
func applyLimitOrderCreate(ctx *Context, owner string, orderID uint32, amountToSell asset.Asset, sellPrice asset.Price, expiration uint32, fillOrKill bool) error {
	// Pre-HF6 hived masks orderID to 16 bits AT LOOKUP TIME ONLY
	// (libraries/chain/database_market.cpp:51-56 — find_limit_order
	// applies the mask before the lookup; the create call passes the
	// raw o.orderid into the limit_order_object constructor at
	// hive_evaluator_transfer.cpp:542). So pre-HF6 we mask the
	// duplicate-detection lookup, but STORE the raw orderID. Post-HF6
	// the same raw orderID is found unmasked.
	lookupOrderID := orderID
	if !ctx.Hardforks.Has(hardfork.HF6) {
		lookupOrderID &= 0xFFFF
	}
	// Sanity: caller must have set sell_price.base.symbol == amount_to_sell.symbol
	// (C++ limit_order_object ctor asserts this).
	if amountToSell.Symbol.Num != sellPrice.Base.Symbol.Num {
		return fmt.Errorf("%w: amount_to_sell symbol must equal sell_price.base symbol", ErrLimitOrderSameSymbol)
	}
	if amountToSell.Symbol.Num == sellPrice.Quote.Symbol.Num {
		return ErrLimitOrderSameSymbol
	}
	if amountToSell.Amount <= 0 || sellPrice.Base.Amount <= 0 || sellPrice.Quote.Amount <= 0 {
		return ErrTransferAmountNotPos
	}
	// Mirror C++ validate_asset_greater_than_zero(amount_to_sell *
	// exchange_rate, "Amount to sell cannot round to 0 when traded")
	// (hive_operations.cpp:639). For variant-1 this is always true (base
	// == amount_to_sell), so the check only fires for create2 when the
	// derived receive rounds to zero.
	if mulDiv(amountToSell.Amount, sellPrice.Quote.Amount, sellPrice.Base.Amount) <= 0 {
		return fmt.Errorf("%w: amount_to_sell rounds to 0 when traded", ErrTransferAmountNotPos)
	}
	// Mirror C++ hive_evaluator_transfer.cpp:536 — strict `expiration >
	// head_block_time` assertion with NO zero-exception. The prior
	// `expiration != 0 &&` guard accepted malformed ops with expiration=0
	// and created orders that would never expire (clearExpiredOrders
	// also skipped Expiration==0 at limit_orders.go:33).
	if expiration <= ctx.HeadBlockTime {
		return ErrLimitOrderExpired
	}
	// hive_evaluator_transfer.cpp:548-551 (create) and :588-591 (create2):
	// from HF20 on the expiration may not exceed head_block_time +
	// HIVE_MAX_LIMIT_ORDER_EXPIRATION (28 days). Pre-HF20 the
	// cappedExpiration* helpers clamp instead. This assert was documented
	// as existing here but had never been written, so gopherd admitted
	// and relayed orders every hived node rejects.
	if ctx.Hardforks.Has(hardfork.HF20) && expiration > ctx.HeadBlockTime+maxLimitOrderExpirationSec {
		return fmt.Errorf("%w: expiration %d exceeds head time + 28 days", ErrLimitOrderExpired, expiration)
	}
	// Both legs must be in the HIVE:HBD market (hive_operations.cpp:601+620
	// in validate(); re-checked in the evaluator because the canonical replay
	// path does not run op.Validate()). Without it a malformed
	// sell_price.quote.symbol silently drops the credit in fillLimitOrder's
	// HIVE/HBD switch.
	//
	// HF29 FIX_LIMIT_ORDER_ASSETS (hive_evaluator_transfer.cpp:540-546 and
	// :578-584) is what makes this a CONSENSUS rule. Until then hived checks
	// it only when in control, so a block carrying an order for a nonexistent
	// asset is accepted by every replaying/syncing hived node. We used to
	// enforce it unconditionally, which is strictly stricter than the network:
	// such a block would have wedged us off the chain while hived followed it.
	// That is the same failure class as the HF28 redundant-signature wall —
	// enforcing a rule hived does not — and it is invisible to replay testing,
	// because a rule no historical block violates looks identical to a rule
	// that is correctly gated.
	//
	// Combined with the base/quote symbol invariants asserted above
	// (amount_to_sell == price.base and amount_to_sell != price.quote), the
	// two checks below are equivalent to hived's explicit pair form.
	if hf29Rule(ctx, hardfork.HF29) {
		if amountToSell.Symbol.Num != asset.HiveSymbol && amountToSell.Symbol.Num != asset.HbdSymbol {
			return fmt.Errorf("%w: amount_to_sell must be HIVE or HBD", ErrUnsupportedSymbol)
		}
		if sellPrice.Quote.Symbol.Num != asset.HiveSymbol && sellPrice.Quote.Symbol.Num != asset.HbdSymbol {
			return fmt.Errorf("%w: sell_price.quote must be HIVE or HBD", ErrUnsupportedSymbol)
		}
	}

	a, err := objects.GetAccount(ctx.DB, owner)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: owner=%q", ErrAccountNotFound, owner)
		}
		return err
	}
	if ghiveDiffOwnerMatch(owner) || ghiveDiffWide(ctx.BlockNum) {
		ghiveDiff("GHIVEDIFF BAL-CREATE block=%d t=%d owner=%s hive=%d hbd=%d need=%d sym=%d\n",
			ctx.BlockNum, ctx.HeadBlockTime, owner, a.Balance, a.HBDBalance, amountToSell.Amount, amountToSell.Symbol.Num)
	}
	switch amountToSell.Symbol.Num {
	case asset.HiveSymbol:
		if a.Balance < amountToSell.Amount {
			return fmt.Errorf("%w: %q has %d HIVE, sell %d", ErrInsufficientBalance, owner, a.Balance, amountToSell.Amount)
		}
		a.Balance -= amountToSell.Amount
	case asset.HbdSymbol:
		// C++ adjust_balance asserts pre-modify balance ≥ |delta| BEFORE
		// the interest-accrual modify lambda runs (database.cpp:3311),
		// so this check uses the pre-accrual HBDBalance — same as C++.
		if a.HBDBalance < amountToSell.Amount {
			return fmt.Errorf("%w: %q has %d HBD, sell %d", ErrInsufficientBalance, owner, a.HBDBalance, amountToSell.Amount)
		}
		objects.AdjustHBDBalance(ctx.DB, &a, ctx.HeadBlockTime, -amountToSell.Amount)
	default:
		// UNCONDITIONAL, and it must stay that way. hived reaches this case
		// through adjust_balance, which asserts on anything that is not HIVE
		// or HBD (database.cpp:3189, `FC_ASSERT( delta.symbol.asset_num ==
		// HIVE_ASSET_NUM_HBD ) && "liquid", "invalid symbol"`), so an order
		// selling a nonexistent asset has ALWAYS been rejected — pre-HF29
		// included, just later in the evaluator than HF29's market check.
		//
		// Without this arm the switch silently matched nothing: no balance was
		// deducted and the order was still created, minting an order out of
		// thin air. That was latent while the HIVE/HBD market check above was
		// unconditional; gating that check on HF29 to match hived is what would
		// have exposed it. The HF29 check is an EARLIER, clearer rejection of
		// something already refused here — not the only thing refusing it.
		return fmt.Errorf("%w: amount_to_sell must be HIVE or HBD, got symbol %d",
			ErrUnsupportedSymbol, amountToSell.Symbol.Num)
	}

	if has, err := objects.HasLimitOrder(ctx.DB, owner, lookupOrderID); err != nil {
		return err
	} else if has {
		return fmt.Errorf("%w: %q/%d", ErrLimitOrderExists, owner, orderID)
	}
	objects.PutAccount(ctx.DB, &a)
	// Allocate the auto-incrementing limit_order_object::id from dgpo.
	// This becomes the by_price tie-break, matching C++'s
	// composite_key<sell_price desc, id asc>.
	limitOrderID := uint64(0)
	if dgpo, derr := objects.GetDGPO(ctx.DB); derr == nil {
		limitOrderID = dgpo.NextLimitOrderID
		dgpo.NextLimitOrderID++
		objects.PutDGPO(ctx.DB, &dgpo)
	}
	newOrder := objects.LimitOrder{
		Owner:         owner,
		OrderID:       orderID,
		Created:       ctx.HeadBlockTime,
		Expiration:    expiration,
		ForSaleAmount: amountToSell.Amount,
		ForSaleSymbol: uint32(amountToSell.Symbol.Num),
		// MinReceiveAmt + SellPriceBaseAmount form the IMMUTABLE original
		// sell_price ratio (sell_price.quote.amount and
		// sell_price.base.amount respectively). For variant-1 those are
		// always equal to (min_to_receive.amount, amount_to_sell.amount).
		// For variant-2 they're (exchange_rate.quote.amount,
		// exchange_rate.base.amount) — which can differ from amount_to_sell
		// when the user picks an exchange_rate not in lowest terms relative
		// to amount_to_sell.
		MinReceiveAmt:       sellPrice.Quote.Amount,
		MinReceiveSym:       uint32(sellPrice.Quote.Symbol.Num),
		SellPriceBaseAmount: sellPrice.Base.Amount,
		ID:                  limitOrderID,
	}
	objects.PutLimitOrder(ctx.DB, &newOrder)

	// apply_order: walk opposite-side orders in by_price descending and
	// match against any whose price satisfies new's max acceptable rate.
	// Returns true when the new order was fully filled.
	filled := applyOrder(ctx, &newOrder)
	if fillOrKill && !filled {
		return fmt.Errorf("limit_order_create: fill_or_kill order %q/%d not fully matched", owner, orderID)
	}
	return nil
}

// limitOrderCancelEvaluator implements limit_order_cancel.
type limitOrderCancelEvaluator struct{}

func (limitOrderCancelEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagLimitOrderCancel {
		return fmt.Errorf("limitOrderCancelEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	lo := opArg.AsLimitOrderCancel()
	orderID := lo.OrderID
	if !ctx.Hardforks.Has(hardfork.HF6) {
		orderID &= 0xFFFF
	}
	if ghiveDiffOwnerMatch(lo.Owner) {
		_, gerr := objects.GetLimitOrder(ctx.DB, lo.Owner, orderID)
		var forSale int64
		present := 0
		if gerr == nil {
			present = 1
			if o2, e2 := objects.GetLimitOrder(ctx.DB, lo.Owner, orderID); e2 == nil {
				forSale = o2.ForSaleAmount
			}
		}
		ghiveDiff("GHIVEDIFF CANCEL block=%d t=%d owner=%s orderid=%d present=%d for_sale=%d\n",
			ctx.BlockNum, ctx.HeadBlockTime, lo.Owner, lo.OrderID, present, forSale)
	}
	order, err := objects.GetLimitOrder(ctx.DB, lo.Owner, orderID)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: %q/%d", ErrLimitOrderNotFound, lo.Owner, lo.OrderID)
		}
		return err
	}
	a, err := objects.GetAccount(ctx.DB, lo.Owner)
	if err != nil {
		return err
	}
	switch asset.AssetNum(order.ForSaleSymbol) {
	case asset.HiveSymbol:
		a.Balance += order.ForSaleAmount
	case asset.HbdSymbol:
		objects.AdjustHBDBalance(ctx.DB, &a, ctx.HeadBlockTime, order.ForSaleAmount)
	}
	objects.PutAccount(ctx.DB, &a)
	// database_market.cpp:81 — limit_order_cancelled with the refunded amount.
	ctx.EmitLimitOrderCancelled(string(lo.Owner), orderID, order.ForSaleAmount, asset.AssetNum(order.ForSaleSymbol) == asset.HbdSymbol)
	objects.DeleteLimitOrderFull(ctx.DB, &order)
	return nil
}

func init() {
	register(op.TagLimitOrderCreate, limitOrderCreateEvaluator{})
	register(op.TagLimitOrderCreate2, limitOrderCreate2Evaluator{})
	register(op.TagLimitOrderCancel, limitOrderCancelEvaluator{})
}
