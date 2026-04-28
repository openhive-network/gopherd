package evaluator

import (
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// transferToVestingEvaluator implements transfer_to_vesting (a.k.a. "power
// up"). The sender's HIVE is converted to VESTS and credited to the
// recipient at the current vesting price. The conversion price uses the
// dynamic_global_property_object's total_vesting_shares /
// total_vesting_fund_hive ratio.
//
// For our minimal-viable schema the DGPO doesn't yet track
// total_vesting_fund_hive separately, so we use a fixed 1:1 conversion
// when no ratio is available. Once vesting accounting lands the real
// price will be substituted.
type transferToVestingEvaluator struct{}

func (transferToVestingEvaluator) Apply(ctx *Context, o op.Operation) error {
	tv, ok := o.(op.TransferToVesting)
	if !ok {
		return fmt.Errorf("transferToVestingEvaluator: unexpected op type %T", o)
	}
	if tv.Amount.Symbol.Num != asset.HiveSymbol {
		return fmt.Errorf("%w: transfer_to_vesting requires HIVE", ErrUnsupportedSymbol)
	}
	if tv.Amount.Amount <= 0 {
		return ErrTransferAmountNotPos
	}

	from, err := objects.GetAccount(ctx.DB, tv.From)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: from=%q", ErrAccountNotFound, tv.From)
		}
		return err
	}
	if from.Balance < tv.Amount.Amount {
		return fmt.Errorf("%w: %q has %d HIVE, needs %d", ErrInsufficientBalance, tv.From, from.Balance, tv.Amount.Amount)
	}

	// to defaults to from when empty — matches hived's behavior.
	toName := tv.To
	if toName == "" {
		toName = tv.From
	}

	from.Balance -= tv.Amount.Amount
	objects.PutAccount(ctx.DB, &from)

	to, err := objects.GetAccount(ctx.DB, toName)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: to=%q", ErrAccountNotFound, toName)
		}
		return err
	}
	// Placeholder 1:1 vesting price. A real conversion uses
	// total_vesting_shares / total_vesting_fund_hive.
	to.VestingShares += tv.Amount.Amount
	objects.PutAccount(ctx.DB, &to)

	// Track the chain-wide totals.
	dgpo, err := objects.GetDGPO(ctx.DB)
	if err != nil && err != state.ErrNotFound {
		return err
	}
	dgpo.CurrentSupply -= tv.Amount.Amount
	dgpo.TotalVestingShares += tv.Amount.Amount
	objects.PutDGPO(ctx.DB, &dgpo)
	return nil
}

func init() {
	register(op.TagTransferToVesting, transferToVestingEvaluator{})
}
