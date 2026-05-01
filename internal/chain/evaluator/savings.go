package evaluator

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_SAVINGS_WITHDRAW_TIME from libraries/protocol/include/hive/protocol/config.hpp:
// 3 days = 60 * 60 * 24 * 3 seconds.
const savingsWithdrawTimeSec = 60 * 60 * 24 * 3

// Errors specific to savings.
var (
	ErrSavingsWithdrawExists   = errors.New("savings withdraw with this request_id already exists")
	ErrSavingsWithdrawNotFound = errors.New("savings withdraw not found")
)

// transferToSavingsEvaluator implements transfer_to_savings.
type transferToSavingsEvaluator struct{}

func (transferToSavingsEvaluator) Apply(ctx *Context, o op.Operation) error {
	t, ok := o.(op.TransferToSavings)
	if !ok {
		return fmt.Errorf("transferToSavingsEvaluator: unexpected op type %T", o)
	}
	if t.Amount.Amount <= 0 {
		return ErrTransferAmountNotPos
	}
	from, err := objects.GetAccount(ctx.DB, t.From)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: from=%q", ErrAccountNotFound, t.From)
		}
		return err
	}
	switch t.Amount.Symbol.Num {
	case asset.HiveSymbol:
		if from.Balance < t.Amount.Amount {
			return fmt.Errorf("%w: %q has %d HIVE", ErrInsufficientBalance, t.From, from.Balance)
		}
		from.Balance -= t.Amount.Amount
	case asset.HbdSymbol:
		if from.HBDBalance < t.Amount.Amount {
			return fmt.Errorf("%w: %q has %d HBD", ErrInsufficientBalance, t.From, from.HBDBalance)
		}
		objects.AdjustHBDBalance(ctx.DB, &from, ctx.HeadBlockTime, -t.Amount.Amount)
	default:
		return fmt.Errorf("%w: transfer_to_savings", ErrUnsupportedSymbol)
	}
	objects.PutAccount(ctx.DB, &from)

	to, err := objects.GetAccount(ctx.DB, t.To)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: to=%q", ErrAccountNotFound, t.To)
		}
		return err
	}
	switch t.Amount.Symbol.Num {
	case asset.HiveSymbol:
		to.SavingsBalance += t.Amount.Amount
	case asset.HbdSymbol:
		objects.AdjustSavingsHBDBalance(ctx.DB, &to, ctx.HeadBlockTime, t.Amount.Amount)
	}
	objects.PutAccount(ctx.DB, &to)
	return nil
}

// transferFromSavingsEvaluator implements transfer_from_savings — schedules
// the funds to land in `to` after savingsWithdrawTimeSec elapses.
type transferFromSavingsEvaluator struct{}

func (transferFromSavingsEvaluator) Apply(ctx *Context, o op.Operation) error {
	t, ok := o.(op.TransferFromSavings)
	if !ok {
		return fmt.Errorf("transferFromSavingsEvaluator: unexpected op type %T", o)
	}
	if t.Amount.Amount <= 0 {
		return ErrTransferAmountNotPos
	}
	from, err := objects.GetAccount(ctx.DB, t.From)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: from=%q", ErrAccountNotFound, t.From)
		}
		return err
	}
	// hived verifies the recipient exists at op time
	// (libraries/chain/hive_evaluator_transfer.cpp:592).
	if has, herr := objects.HasAccount(ctx.DB, t.To); herr != nil {
		return herr
	} else if !has {
		return fmt.Errorf("%w: to=%q", ErrAccountNotFound, t.To)
	}
	switch t.Amount.Symbol.Num {
	case asset.HiveSymbol:
		if from.SavingsBalance < t.Amount.Amount {
			return fmt.Errorf("%w: %q savings %d HIVE", ErrInsufficientBalance, t.From, from.SavingsBalance)
		}
		from.SavingsBalance -= t.Amount.Amount
	case asset.HbdSymbol:
		if from.SavingsHBDBalance < t.Amount.Amount {
			return fmt.Errorf("%w: %q savings %d HBD", ErrInsufficientBalance, t.From, from.SavingsHBDBalance)
		}
		objects.AdjustSavingsHBDBalance(ctx.DB, &from, ctx.HeadBlockTime, -t.Amount.Amount)
	default:
		return fmt.Errorf("%w: transfer_from_savings", ErrUnsupportedSymbol)
	}
	if has, err := objects.HasSavingsWithdraw(ctx.DB, t.From, t.RequestID); err != nil {
		return err
	} else if has {
		return fmt.Errorf("%w: %q/%d", ErrSavingsWithdrawExists, t.From, t.RequestID)
	}
	objects.PutAccount(ctx.DB, &from)
	objects.PutSavingsWithdraw(ctx.DB, &objects.SavingsWithdraw{
		From:      t.From,
		RequestID: t.RequestID,
		To:        t.To,
		Amount:    t.Amount.Amount,
		Symbol:    uint32(t.Amount.Symbol.Num),
		Memo:      t.Memo,
		Complete:  ctx.HeadBlockTime + savingsWithdrawTimeSec,
	})
	// NOTE: the withdraw doesn't auto-finalize after 3 days here. Hived has
	// a per-block "process_funds" / "process_savings_withdrawals" that
	// completes mature requests; that lands with the per-block hooks.
	return nil
}

// cancelTransferFromSavingsEvaluator implements cancel_transfer_from_savings.
type cancelTransferFromSavingsEvaluator struct{}

func (cancelTransferFromSavingsEvaluator) Apply(ctx *Context, o op.Operation) error {
	c, ok := o.(op.CancelTransferFromSavings)
	if !ok {
		return fmt.Errorf("cancelTransferFromSavingsEvaluator: unexpected op type %T", o)
	}
	w, err := objects.GetSavingsWithdraw(ctx.DB, c.From, c.RequestID)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: %q/%d", ErrSavingsWithdrawNotFound, c.From, c.RequestID)
		}
		return err
	}
	from, err := objects.GetAccount(ctx.DB, c.From)
	if err != nil {
		return err
	}
	switch asset.AssetNum(w.Symbol) {
	case asset.HiveSymbol:
		from.SavingsBalance += w.Amount
	case asset.HbdSymbol:
		objects.AdjustSavingsHBDBalance(ctx.DB, &from, ctx.HeadBlockTime, w.Amount)
	}
	objects.PutAccount(ctx.DB, &from)
	objects.DeleteSavingsWithdrawFull(ctx.DB, &w)
	return nil
}

func init() {
	register(op.TagTransferToSavings, transferToSavingsEvaluator{})
	register(op.TagTransferFromSavings, transferFromSavingsEvaluator{})
	register(op.TagCancelTransferFromSavings, cancelTransferFromSavingsEvaluator{})
}
