package evaluator

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Errors specific to limit_order evaluators.
var (
	ErrLimitOrderExists      = errors.New("limit order with this orderid already exists")
	ErrLimitOrderNotFound    = errors.New("limit order not found")
	ErrLimitOrderExpired     = errors.New("limit order expiration has passed")
	ErrLimitOrderSameSymbol  = errors.New("limit order amount_to_sell and min_to_receive must differ in symbol")
)

// limitOrderCreateEvaluator implements limit_order_create.
//
// Hived semantics: validate, debit owner.balance by amount_to_sell, run
// the matching engine against existing orders on the opposite side,
// create a limit_order_object with whatever's unfilled. We elide the
// matching engine for now — the order is debited and recorded; cross-
// trades won't happen until the engine lands.
type limitOrderCreateEvaluator struct{}

func (limitOrderCreateEvaluator) Apply(ctx *Context, o op.Operation) error {
	lo, ok := o.(op.LimitOrderCreate)
	if !ok {
		return fmt.Errorf("limitOrderCreateEvaluator: unexpected op type %T", o)
	}
	return applyLimitOrderCreate(ctx, lo.Owner, lo.OrderID, lo.AmountToSell, lo.MinToReceive, lo.Expiration, lo.FillOrKill)
}

type limitOrderCreate2Evaluator struct{}

func (limitOrderCreate2Evaluator) Apply(ctx *Context, o op.Operation) error {
	lo, ok := o.(op.LimitOrderCreate2)
	if !ok {
		return fmt.Errorf("limitOrderCreate2Evaluator: unexpected op type %T", o)
	}
	// In create2 the price implies min_to_receive: amount_to_sell * (quote/base).
	// We compute it via the Price's base/quote ratio.
	if lo.ExchangeRate.Base.Amount <= 0 || lo.ExchangeRate.Quote.Amount <= 0 {
		return fmt.Errorf("%w: invalid exchange_rate", ErrTransferAmountNotPos)
	}
	// min_to_receive = amount_to_sell * exchange_rate.quote / exchange_rate.base
	num := lo.AmountToSell.Amount * lo.ExchangeRate.Quote.Amount
	minToReceiveAmt := num / lo.ExchangeRate.Base.Amount
	minToReceive := asset.Asset{
		Amount: minToReceiveAmt,
		Symbol: lo.ExchangeRate.Quote.Symbol,
	}
	return applyLimitOrderCreate(ctx, lo.Owner, lo.OrderID, lo.AmountToSell, minToReceive, lo.Expiration, lo.FillOrKill)
}

func applyLimitOrderCreate(ctx *Context, owner string, orderID uint32, amountToSell, minToReceive asset.Asset, expiration uint32, fillOrKill bool) error {
	if amountToSell.Symbol.Num == minToReceive.Symbol.Num {
		return ErrLimitOrderSameSymbol
	}
	if amountToSell.Amount <= 0 || minToReceive.Amount <= 0 {
		return ErrTransferAmountNotPos
	}
	if expiration != 0 && expiration <= ctx.BlockTime {
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
		a.HBDBalance -= amountToSell.Amount
	}

	if has, err := objects.HasLimitOrder(ctx.DB, owner, orderID); err != nil {
		return err
	} else if has {
		return fmt.Errorf("%w: %q/%d", ErrLimitOrderExists, owner, orderID)
	}
	objects.PutAccount(ctx.DB, &a)
	objects.PutLimitOrder(ctx.DB, &objects.LimitOrder{
		Owner:         owner,
		OrderID:       orderID,
		Created:       ctx.BlockTime,
		Expiration:    expiration,
		ForSaleAmount: amountToSell.Amount,
		ForSaleSymbol: uint32(amountToSell.Symbol.Num),
		MinReceiveAmt: minToReceive.Amount,
		MinReceiveSym: uint32(minToReceive.Symbol.Num),
	})

	// fill_or_kill fully-or-nothing semantics require running the matching
	// engine; until that lands we accept (don't fail) FoK orders. They
	// behave as ordinary orders that wait for a counterparty.
	_ = fillOrKill
	return nil
}

// limitOrderCancelEvaluator implements limit_order_cancel.
type limitOrderCancelEvaluator struct{}

func (limitOrderCancelEvaluator) Apply(ctx *Context, o op.Operation) error {
	lo, ok := o.(op.LimitOrderCancel)
	if !ok {
		return fmt.Errorf("limitOrderCancelEvaluator: unexpected op type %T", o)
	}
	order, err := objects.GetLimitOrder(ctx.DB, lo.Owner, lo.OrderID)
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
		a.HBDBalance += order.ForSaleAmount
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
