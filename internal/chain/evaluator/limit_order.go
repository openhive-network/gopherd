package evaluator

import (
	"errors"
	"fmt"
	"os"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// ghiveDiff emits a GHIVEDIFF-tagged stderr line whenever the GHIVEDIFF
// env var is set, matching the format of the patched hived in
// libraries/chain/database_market.cpp + hive_evaluator_transfer.cpp. Used
// to side-by-side diff replay traces. Filtered to adm/abit by default.
func ghiveDiff(format string, args ...any) {
	if os.Getenv("GHIVEDIFF") == "" {
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
	ErrLimitOrderExists      = errors.New("limit order with this orderid already exists")
	ErrLimitOrderNotFound    = errors.New("limit order not found")
	ErrLimitOrderExpired     = errors.New("limit order expiration has passed")
	ErrLimitOrderSameSymbol  = errors.New("limit order amount_to_sell and min_to_receive must differ in symbol")
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

func (limitOrderCreateEvaluator) Apply(ctx *Context, o op.Operation) error {
	lo, ok := o.(op.LimitOrderCreate)
	if !ok {
		return fmt.Errorf("limitOrderCreateEvaluator: unexpected op type %T", o)
	}
	if ghiveDiffOwnerMatch(lo.Owner) || ghiveDiffWide(ctx.BlockNum) {
		ghiveDiff("GHIVEDIFF CREATE block=%d t=%d owner=%s orderid=%d sell=%d/%d recv=%d/%d exp=%d\n",
			ctx.BlockNum, ctx.HeadBlockTime, lo.Owner, lo.OrderID,
			lo.AmountToSell.Amount, lo.AmountToSell.Symbol.Num,
			lo.MinToReceive.Amount, lo.MinToReceive.Symbol.Num,
			lo.Expiration)
	}
	err := applyLimitOrderCreate(ctx, lo.Owner, lo.OrderID, lo.AmountToSell, lo.MinToReceive, lo.Expiration, lo.FillOrKill)
	if err != nil && (ghiveDiffOwnerMatch(lo.Owner) || ghiveDiffWide(ctx.BlockNum)) {
		ghiveDiff("GHIVEDIFF CREATE-FAIL block=%d t=%d owner=%s orderid=%d err=%v\n",
			ctx.BlockNum, ctx.HeadBlockTime, lo.Owner, lo.OrderID, err)
	}
	return err
}

type limitOrderCreate2Evaluator struct{}

func (limitOrderCreate2Evaluator) Apply(ctx *Context, o op.Operation) error {
	lo, ok := o.(op.LimitOrderCreate2)
	if !ok {
		return fmt.Errorf("limitOrderCreate2Evaluator: unexpected op type %T", o)
	}
	if ghiveDiffOwnerMatch(lo.Owner) || ghiveDiffWide(ctx.BlockNum) {
		ghiveDiff("GHIVEDIFF CREATE2 block=%d t=%d owner=%s orderid=%d sell=%d/%d rate=%d/%d/%d/%d exp=%d\n",
			ctx.BlockNum, ctx.HeadBlockTime, lo.Owner, lo.OrderID,
			lo.AmountToSell.Amount, lo.AmountToSell.Symbol.Num,
			lo.ExchangeRate.Base.Amount, lo.ExchangeRate.Base.Symbol.Num,
			lo.ExchangeRate.Quote.Amount, lo.ExchangeRate.Quote.Symbol.Num,
			lo.Expiration)
	}
	// In create2 the price implies min_to_receive: amount_to_sell * (quote/base).
	// We compute it via the Price's base/quote ratio.
	if lo.ExchangeRate.Base.Amount <= 0 || lo.ExchangeRate.Quote.Amount <= 0 {
		return fmt.Errorf("%w: invalid exchange_rate", ErrTransferAmountNotPos)
	}
	// min_to_receive = amount_to_sell * exchange_rate.quote / exchange_rate.base.
	// hived uses fc::uint128 here (asset::operator*(price)): amounts × prices
	// can blow past int64 well before mainnet caps.
	minToReceiveAmt := mulDiv(lo.AmountToSell.Amount, lo.ExchangeRate.Quote.Amount, lo.ExchangeRate.Base.Amount)
	minToReceive := asset.Asset{
		Amount: minToReceiveAmt,
		Symbol: lo.ExchangeRate.Quote.Symbol,
	}
	return applyLimitOrderCreate(ctx, lo.Owner, lo.OrderID, lo.AmountToSell, minToReceive, lo.Expiration, lo.FillOrKill)
}

func applyLimitOrderCreate(ctx *Context, owner string, orderID uint32, amountToSell, minToReceive asset.Asset, expiration uint32, fillOrKill bool) error {
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
	if amountToSell.Symbol.Num == minToReceive.Symbol.Num {
		return ErrLimitOrderSameSymbol
	}
	if amountToSell.Amount <= 0 || minToReceive.Amount <= 0 {
		return ErrTransferAmountNotPos
	}
	if expiration != 0 && expiration <= ctx.HeadBlockTime {
		return ErrLimitOrderExpired
	}
	if amountToSell.Symbol.Num != asset.HiveSymbol && amountToSell.Symbol.Num != asset.HbdSymbol {
		return fmt.Errorf("%w: amount_to_sell must be HIVE or HBD", ErrUnsupportedSymbol)
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
		if a.HBDBalance < amountToSell.Amount {
			return fmt.Errorf("%w: %q has %d HBD, sell %d", ErrInsufficientBalance, owner, a.HBDBalance, amountToSell.Amount)
		}
		objects.AdjustHBDBalance(ctx.DB, &a, ctx.HeadBlockTime, -amountToSell.Amount)
	}

	if has, err := objects.HasLimitOrder(ctx.DB, owner, lookupOrderID); err != nil {
		return err
	} else if has {
		return fmt.Errorf("%w: %q/%d", ErrLimitOrderExists, owner, orderID)
	}
	objects.PutAccount(ctx.DB, &a)
	newOrder := objects.LimitOrder{
		Owner:               owner,
		OrderID:             orderID,
		Created:             ctx.HeadBlockTime,
		Expiration:          expiration,
		ForSaleAmount:       amountToSell.Amount,
		ForSaleSymbol:       uint32(amountToSell.Symbol.Num),
		MinReceiveAmt:       minToReceive.Amount,
		MinReceiveSym:       uint32(minToReceive.Symbol.Num),
		SellPriceBaseAmount: amountToSell.Amount, // immutable, set at create time
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

func (limitOrderCancelEvaluator) Apply(ctx *Context, o op.Operation) error {
	lo, ok := o.(op.LimitOrderCancel)
	if !ok {
		return fmt.Errorf("limitOrderCancelEvaluator: unexpected op type %T", o)
	}
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
	objects.DeleteLimitOrderFull(ctx.DB, &order)
	return nil
}

func init() {
	register(op.TagLimitOrderCreate, limitOrderCreateEvaluator{})
	register(op.TagLimitOrderCreate2, limitOrderCreate2Evaluator{})
	register(op.TagLimitOrderCancel, limitOrderCancelEvaluator{})
}
