package evaluator

import (
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_VESTING_WITHDRAW_INTERVAL_SECONDS — 1 week between payouts.
const vestingWithdrawIntervalSec = 60 * 60 * 24 * 7

// HIVE_VESTING_WITHDRAW_INTERVALS_PRE_HF_16 — 104 weeks (~2 years).
// HIVE_VESTING_WITHDRAW_INTERVALS                  — 13 weeks (1 quarter), HF16 onwards.
const (
	vestingWithdrawIntervalsPreHF16 = 104
	vestingWithdrawIntervals        = 13
)

// withdrawVestingEvaluator implements withdraw_vesting (start power-down).
// Setting vesting_shares=0 cancels an existing power-down.
type withdrawVestingEvaluator struct{}

func (withdrawVestingEvaluator) Apply(ctx *Context, o op.Operation) error {
	w, ok := o.(op.WithdrawVesting)
	if !ok {
		return fmt.Errorf("withdrawVestingEvaluator: unexpected op type %T", o)
	}
	if w.VestingShares.Symbol.Num != asset.VestsSymbol {
		return fmt.Errorf("%w: withdraw_vesting requires VESTS", ErrUnsupportedSymbol)
	}
	if w.VestingShares.Amount < 0 {
		return ErrTransferAmountNotPos
	}

	a, err := objects.GetAccount(ctx.DB, w.Account)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: account=%q", ErrAccountNotFound, w.Account)
		}
		return err
	}

	// Drop any existing schedule entry for this account from the
	// time-ordered index before we mutate the next-withdrawal field.
	if a.NextVestingWithdrawal != 0 {
		objects.DeleteVestingWithdrawalSchedule(ctx.DB, a.NextVestingWithdrawal, w.Account)
	}

	if w.VestingShares.Amount == 0 {
		// Cancel the schedule.
		a.VestingWithdrawRate = 0
		a.NextVestingWithdrawal = 0
		a.ToWithdraw = 0
		a.Withdrawn = 0
		objects.PutAccount(ctx.DB, &a)
		return nil
	}

	available := a.VestingShares - a.DelegatedVestingShares
	if available < w.VestingShares.Amount {
		return fmt.Errorf("%w: %q has %d undelegated VESTS", ErrInsufficientBalance, w.Account, available)
	}

	intervals := int64(vestingWithdrawIntervalsPreHF16)
	if ctx.Hardforks.Has(hardfork.HF16) {
		intervals = vestingWithdrawIntervals
	}
	a.VestingWithdrawRate = w.VestingShares.Amount / intervals
	if a.VestingWithdrawRate == 0 {
		a.VestingWithdrawRate = 1
	}
	a.NextVestingWithdrawal = ctx.BlockTime + vestingWithdrawIntervalSec
	a.ToWithdraw = w.VestingShares.Amount
	a.Withdrawn = 0
	objects.PutAccount(ctx.DB, &a)
	objects.PutVestingWithdrawalSchedule(ctx.DB, a.NextVestingWithdrawal, w.Account)
	return nil
}

// setWithdrawVestingRouteEvaluator implements set_withdraw_vesting_route.
type setWithdrawVestingRouteEvaluator struct{}

func (setWithdrawVestingRouteEvaluator) Apply(ctx *Context, o op.Operation) error {
	r, ok := o.(op.SetWithdrawVestingRoute)
	if !ok {
		return fmt.Errorf("setWithdrawVestingRouteEvaluator: unexpected op type %T", o)
	}
	if has, err := objects.HasAccount(ctx.DB, r.FromAccount); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: from=%q", ErrAccountNotFound, r.FromAccount)
	}
	if has, err := objects.HasAccount(ctx.DB, r.ToAccount); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: to=%q", ErrAccountNotFound, r.ToAccount)
	}
	if r.Percent == 0 {
		objects.DeleteWithdrawRoute(ctx.DB, r.FromAccount, r.ToAccount)
		return nil
	}
	objects.PutWithdrawRoute(ctx.DB, &objects.WithdrawRoute{
		From:     r.FromAccount,
		To:       r.ToAccount,
		Percent:  r.Percent,
		AutoVest: r.AutoVest,
	})
	return nil
}

// delegateVestingSharesEvaluator implements delegate_vesting_shares.
type delegateVestingSharesEvaluator struct{}

func (delegateVestingSharesEvaluator) Apply(ctx *Context, o op.Operation) error {
	d, ok := o.(op.DelegateVestingShares)
	if !ok {
		return fmt.Errorf("delegateVestingSharesEvaluator: unexpected op type %T", o)
	}
	if d.VestingShares.Symbol.Num != asset.VestsSymbol {
		return fmt.Errorf("%w: delegate_vesting_shares requires VESTS", ErrUnsupportedSymbol)
	}
	if d.VestingShares.Amount < 0 {
		return ErrTransferAmountNotPos
	}
	if d.Delegator == d.Delegatee {
		return fmt.Errorf("delegate_vesting_shares: cannot delegate to self")
	}

	delegator, err := objects.GetAccount(ctx.DB, d.Delegator)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: delegator=%q", ErrAccountNotFound, d.Delegator)
		}
		return err
	}
	delegatee, err := objects.GetAccount(ctx.DB, d.Delegatee)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: delegatee=%q", ErrAccountNotFound, d.Delegatee)
		}
		return err
	}

	prev, err := objects.GetVestingDelegation(ctx.DB, d.Delegator, d.Delegatee)
	prevAmt := int64(0)
	if err == nil {
		prevAmt = prev.VestingShares
	} else if err != state.ErrNotFound {
		return err
	}
	delta := d.VestingShares.Amount - prevAmt

	if delta > 0 {
		// Increasing delegation requires the delegator's available VESTS.
		available := delegator.VestingShares - delegator.DelegatedVestingShares
		if available < delta {
			return fmt.Errorf("%w: %q has %d undelegated VESTS", ErrInsufficientBalance, d.Delegator, available)
		}
	}
	delegator.DelegatedVestingShares += delta
	delegatee.ReceivedVestingShares += delta
	objects.PutAccount(ctx.DB, &delegator)
	objects.PutAccount(ctx.DB, &delegatee)

	if d.VestingShares.Amount == 0 {
		objects.DeleteVestingDelegation(ctx.DB, d.Delegator, d.Delegatee)
	} else {
		objects.PutVestingDelegation(ctx.DB, &objects.VestingDelegation{
			Delegator:     d.Delegator,
			Delegatee:     d.Delegatee,
			VestingShares: d.VestingShares.Amount,
			MinDelegation: ctx.BlockTime + 60*60*24*5, // ~5 day cooldown
		})
	}
	return nil
}

func init() {
	register(op.TagWithdrawVesting, withdrawVestingEvaluator{})
	register(op.TagSetWithdrawVestingRoute, setWithdrawVestingRouteEvaluator{})
	register(op.TagDelegateVestingShares, delegateVestingSharesEvaluator{})
}
