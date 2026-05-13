package evaluator

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_CONVERSION_DELAY: pre-HF16 it's 7 days; HF16+ is 3.5 days.
// (libraries/protocol/include/hive/protocol/config.hpp)
const (
	conversionDelaySecPreHF16 uint32 = 60 * 60 * 24 * 7    // 7 days
	conversionDelaySec        uint32 = 60 * 60 * 24 * 7 / 2 // 3.5 days
)

func conversionDelay(hf hardfork.Set) uint32 {
	if hf.Has(hardfork.HF16) {
		return conversionDelaySec
	}
	return conversionDelaySecPreHF16
}

// Errors specific to convert.
var (
	ErrConvertExists = errors.New("convert request with this id already exists")
)

type convertEvaluator struct{}

func (convertEvaluator) Apply(ctx *Context, opArg op.Op) error {
	o := opArg.Operation()
	c, ok := o.(op.Convert)
	if !ok {
		return fmt.Errorf("convertEvaluator: unexpected op type %T", o)
	}
	if c.Amount.Symbol.Num != asset.HbdSymbol {
		return fmt.Errorf("%w: convert requires HBD", ErrUnsupportedSymbol)
	}
	if c.Amount.Amount <= 0 {
		return ErrTransferAmountNotPos
	}
	a, err := objects.GetAccount(ctx.DB, c.Owner)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: owner=%q", ErrAccountNotFound, c.Owner)
		}
		return err
	}
	if a.HBDBalance < c.Amount.Amount {
		return fmt.Errorf("%w: %q has %d HBD, convert %d", ErrInsufficientBalance, c.Owner, a.HBDBalance, c.Amount.Amount)
	}
	if has, err := objects.HasConvertRequest(ctx.DB, c.Owner, c.RequestID); err != nil {
		return err
	} else if has {
		return fmt.Errorf("%w: %q/%d", ErrConvertExists, c.Owner, c.RequestID)
	}
	objects.AdjustHBDBalance(ctx.DB, &a, ctx.HeadBlockTime, -c.Amount.Amount)
	objects.PutAccount(ctx.DB, &a)
	objects.PutConvertRequest(ctx.DB, &objects.ConvertRequest{
		Owner:      c.Owner,
		RequestID:  c.RequestID,
		Amount:     c.Amount.Amount,
		Conversion: ctx.HeadBlockTime + conversionDelay(ctx.Hardforks),
	})
	return nil
}

type collateralizedConvertEvaluator struct{}

func (collateralizedConvertEvaluator) Apply(ctx *Context, opArg op.Op) error {
	o := opArg.Operation()
	c, ok := o.(op.CollateralizedConvert)
	if !ok {
		return fmt.Errorf("collateralizedConvertEvaluator: unexpected op type %T", o)
	}
	// HF1_25 — collateralized_convert is HF25+ only.
	if !ctx.Hardforks.Has(hardfork.HF25) {
		return fmt.Errorf("collateralized_convert: requires HF25")
	}
	if c.Amount.Symbol.Num != asset.HiveSymbol {
		return fmt.Errorf("%w: collateralized_convert requires HIVE collateral", ErrUnsupportedSymbol)
	}
	if c.Amount.Amount <= 0 {
		return ErrTransferAmountNotPos
	}
	a, err := objects.GetAccount(ctx.DB, c.Owner)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: owner=%q", ErrAccountNotFound, c.Owner)
		}
		return err
	}
	if a.Balance < c.Amount.Amount {
		return fmt.Errorf("%w: %q HIVE", ErrInsufficientBalance, c.Owner)
	}
	if has, err := objects.HasCollateralizedConvert(ctx.DB, c.Owner, c.RequestID); err != nil {
		return err
	} else if has {
		return fmt.Errorf("%w: %q/%d", ErrConvertExists, c.Owner, c.RequestID)
	}
	a.Balance -= c.Amount.Amount
	// Hived credits HBD immediately at the median price minus
	// HIVE_COLLATERALIZED_CONVERSION_FEE; we don't have a price feed yet,
	// so we credit a placeholder value of zero and settle nothing. The
	// pending conversion is recorded for the per-block hook to finalize
	// when the price feed lands.
	objects.PutAccount(ctx.DB, &a)
	objects.PutCollateralizedConvert(ctx.DB, &objects.CollateralizedConvert{
		Owner:      c.Owner,
		RequestID:  c.RequestID,
		Collateral: c.Amount.Amount,
		HBDOut:     0,
		Conversion: ctx.HeadBlockTime + conversionDelay(ctx.Hardforks),
	})
	return nil
}

func init() {
	register(op.TagConvert, convertEvaluator{})
	register(op.TagCollateralizedConvert, collateralizedConvertEvaluator{})
}
