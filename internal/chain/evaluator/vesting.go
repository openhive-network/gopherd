package evaluator

import (
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_VESTING_WITHDRAW_INTERVAL_SECONDS — 1 week between payouts at all
// hardforks. The macro is bound to HIVE_CASHOUT_WINDOW_SECONDS but is
// resolved through configuration_data, which never returns the
// PRE_HF12/PRE_HF17 short windows for vesting withdrawals
// (libraries/protocol/include/hive/protocol/config.hpp:238 — "these
// intervals should last the same (1 week by default)"). Only the
// payout COUNT changes across HF16: 104 weeks pre, 13 weeks post.
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
		// Pre-HF20: hived silently returns on negative shares (see
		// hive_evaluator_transfer.cpp:334 referencing block 23847548).
		// HF20+: it's a hard error.
		if ctx.Hardforks.Has(hardfork.HF20) {
			return ErrTransferAmountNotPos
		}
		return nil
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
		return fmt.Errorf("%w: %q has %d undelegated VESTS, requested %d (vesting=%d, delegated=%d)", ErrInsufficientBalance, w.Account, available, w.VestingShares.Amount, a.VestingShares, a.DelegatedVestingShares)
	}

	intervals := int64(vestingWithdrawIntervalsPreHF16)
	if ctx.Hardforks.Has(hardfork.HF16) {
		intervals = vestingWithdrawIntervals
	}
	intervalSec := uint32(vestingWithdrawIntervalSec)
	a.VestingWithdrawRate = w.VestingShares.Amount / intervals
	if a.VestingWithdrawRate == 0 {
		a.VestingWithdrawRate = 1
	}
	a.NextVestingWithdrawal = ctx.HeadBlockTime + intervalSec
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
	if delta >= 0 {
		// Increasing or initial-set: VESTS are locked immediately.
		delegator.DelegatedVestingShares += delta
		delegatee.ReceivedVestingShares += delta
	} else {
		// Decreasing: per HF17 hived schedules the returned VESTS to
		// land back in the delegator's available pool after the
		// DelegationReturnPeriod (default 5 days, HF20+ from DGPO).
		// The delegatee's received pool drops immediately. The
		// delegator's locked pool stays high until the expiration
		// matures (clearExpiredDelegations decrements it then).
		returned := -delta
		delegatee.ReceivedVestingShares -= returned
		if delegatee.ReceivedVestingShares < 0 {
			delegatee.ReceivedVestingShares = 0
		}
		// Compute expiration time. Default to 5 days; if DGPO has a
		// configured DelegationReturnPeriod (HF20+), use that.
		// hive_evaluator_transfer.cpp:857 — the returned vests' expiry
		// is `max(now + delegation_return_period, delegation->min_delegation_time)`.
		// Account-create-with-delegation rows set min_delegation_time to
		// head + 30 days, so reductions taken within those 30 days are
		// pushed out to that anchor instead of returning at the normal
		// 5-day cadence.
		retSec := uint32(5 * 24 * 60 * 60)
		if dgpo, derr := objects.GetDGPO(ctx.DB); derr == nil && dgpo.DelegationReturnPeriod > 0 {
			retSec = dgpo.DelegationReturnPeriod
		}
		expiration := ctx.HeadBlockTime + retSec
		if prev.MinDelegation > expiration {
			expiration = prev.MinDelegation
		}
		// Use the (delegator, blockTime) pair as a coarse ID; if
		// the delegator does multiple reductions in the same block
		// they'll collide. Hived uses a monotonic id; we use a hash
		// of (delegator, blockTime, returned) to keep collisions
		// improbable.
		id := uint32(returned ^ int64(ctx.HeadBlockTime))
		objects.PutVestingDelegationExpiration(ctx.DB, &objects.VestingDelegationExpiration{
			ID:            id,
			Delegator:     d.Delegator,
			VestingShares: returned,
			Expiration:    expiration,
		})
	}
	// HF20+: both sides' effective_vests changed → refresh RC caps.
	if ctx.Hardforks.Has(hardfork.HF20) {
		objects.RegenerateRCMana(&delegator, ctx.HeadBlockTime)
		objects.RegenerateRCMana(&delegatee, ctx.HeadBlockTime)
	}
	objects.PutAccount(ctx.DB, &delegator)
	objects.PutAccount(ctx.DB, &delegatee)

	if d.VestingShares.Amount == 0 {
		objects.DeleteVestingDelegation(ctx.DB, d.Delegator, d.Delegatee)
	} else {
		objects.PutVestingDelegation(ctx.DB, &objects.VestingDelegation{
			Delegator:     d.Delegator,
			Delegatee:     d.Delegatee,
			VestingShares: d.VestingShares.Amount,
			MinDelegation: ctx.HeadBlockTime + 60*60*24*5, // ~5 day cooldown
		})
	}
	return nil
}

func init() {
	register(op.TagWithdrawVesting, withdrawVestingEvaluator{})
	register(op.TagSetWithdrawVestingRoute, setWithdrawVestingRouteEvaluator{})
	register(op.TagDelegateVestingShares, delegateVestingSharesEvaluator{})
}
