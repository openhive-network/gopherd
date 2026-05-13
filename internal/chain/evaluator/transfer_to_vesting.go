package evaluator

import (
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
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

func (transferToVestingEvaluator) Apply(ctx *Context, opArg op.Op) error {
	o := opArg.Operation()
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
	// HF21__3343 — cannot vest on behalf of treasury
	// (hive_evaluator_transfer.cpp:299).
	if ctx.Hardforks.Has(hardfork.HF21) && isTreasuryAccount(tv.To, ctx.Hardforks) {
		return fmt.Errorf("transfer_to_vesting: cannot vest on behalf of treasury %q", tv.To)
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

	// HF20+: refresh sender's RC manabar before any vesting changes,
	// so a stale cap from a previous block doesn't leak through.
	if ctx.Hardforks.Has(hardfork.HF20) {
		objects.RegenerateRCMana(&from, ctx.HeadBlockTime)
	}
	from.Balance -= tv.Amount.Amount
	objects.PutAccount(ctx.DB, &from)

	// Confirm the recipient exists before moving HIVE into the fund.
	if has, err := objects.HasAccount(ctx.DB, toName); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: to=%q", ErrAccountNotFound, toName)
	}

	// HF24+: voting is deferred via delayed_votes — call the no-vote-adjust
	// helper. Pre-HF24: CreateVesting (matches C++ create_vesting2) does
	// the balance update AND propagates the new vest into witness votes.
	var newVests int64
	if ctx.Hardforks.Has(hardfork.HF24) {
		newVests, err = AdjustAccountVestingBalance(ctx.DB, toName, tv.Amount.Amount, false)
	} else {
		newVests, err = CreateVesting(ctx.DB, toName, tv.Amount.Amount, false)
	}
	if err != nil {
		return err
	}
	if newVests <= 0 {
		return nil
	}
	if ctx.Hardforks.Has(hardfork.HF24) {
		to, err := objects.GetAccount(ctx.DB, toName)
		if err != nil {
			return err
		}
		if err := objects.DelayedVotingAddOnAccount(&to, ctx.HeadBlockTime, uint64(newVests)); err != nil {
			return fmt.Errorf("transfer_to_vesting: delayed_voting add: %w", err)
		}
		objects.PutAccount(ctx.DB, &to)
	}
	// HF20+: recipient's effective_vests went up → refresh RC cap.
	if ctx.Hardforks.Has(hardfork.HF20) {
		if to, err := objects.GetAccount(ctx.DB, toName); err == nil {
			objects.RegenerateRCMana(&to, ctx.HeadBlockTime)
			objects.PutAccount(ctx.DB, &to)
		}
	}
	return nil
}

func init() {
	register(op.TagTransferToVesting, transferToVestingEvaluator{})
}
