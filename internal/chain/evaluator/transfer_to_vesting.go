package evaluator

import (
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// transferToVestingEvaluator implements transfer_to_vesting (a.k.a. "power
// up"). The sender's HIVE moves out of liquid balance and into the
// chain-wide vesting fund; the recipient is credited VESTS at the
// current vesting price.
//
// hived's accounting (libraries/chain/hive_evaluator.cpp,
// transfer_to_vesting_evaluator):
//
//	from.balance               -= amount
//	total_vesting_fund_hive    += amount        // HIVE moves into the fund
//	total_vesting_shares       += new_vesting   // VESTS issued at dynamic price
//	to.vesting_shares          += new_vesting
//	current_supply              unchanged       // fund_hive is a subset of supply
//
// The dynamic price is total_vesting_shares / total_vesting_fund_hive
// (with the 1:1000 satoshi-level ratio at genesis). Both writes are
// encapsulated in CreateVesting.
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

	// Confirm the recipient exists before moving HIVE into the fund.
	if has, err := objects.HasAccount(ctx.DB, toName); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: to=%q", ErrAccountNotFound, toName)
	}

	// CreateVesting handles fund_hive += amount, shares += new_vesting,
	// and recipient.vesting_shares += new_vesting at the dynamic price.
	if _, err := CreateVesting(ctx.DB, toName, tv.Amount.Amount, false); err != nil {
		return err
	}
	return nil
}

func init() {
	register(op.TagTransferToVesting, transferToVestingEvaluator{})
}
