package evaluator

import (
	"fmt"
	"math"
	"math/bits"

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

func (withdrawVestingEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagWithdrawVesting {
		return fmt.Errorf("withdrawVestingEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	w := opArg.AsWithdrawVesting()
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
	// hive_evaluator_transfer.cpp:374-377 — HF21+ rounds the rate UP so
	// rate × intervals ≥ requested. Pre-HF21, the integer truncation
	// could leave a few satoshi unwithdrawn at the end.
	if ctx.Hardforks.Has(hardfork.HF21) && a.VestingWithdrawRate*intervals < w.VestingShares.Amount {
		a.VestingWithdrawRate++
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

func (setWithdrawVestingRouteEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagSetWithdrawVestingRoute {
		return fmt.Errorf("setWithdrawVestingRouteEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	r := opArg.AsSetWithdrawVestingRoute()
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
	// HF21__3343 — withdraw target cannot be treasury.
	if ctx.Hardforks.Has(hardfork.HF21) && isTreasuryAccount(r.ToAccount, ctx.Hardforks) {
		return fmt.Errorf("set_withdraw_vesting_route: target cannot be treasury %q", r.ToAccount)
	}
	// hive_evaluator_transfer.cpp:408 — disallow creating a 0% route
	// when none exists. percent==0 with an existing row removes it.
	had, _ := objects.GetWithdrawRoute(ctx.DB, r.FromAccount, r.ToAccount)
	exists := had.From == r.FromAccount && had.To == r.ToAccount
	if r.Percent == 0 {
		if !exists {
			return fmt.Errorf("set_withdraw_vesting_route: cannot create a 0%% destination")
		}
		objects.DeleteWithdrawRoute(ctx.DB, r.FromAccount, r.ToAccount)
	} else {
		objects.PutWithdrawRoute(ctx.DB, &objects.WithdrawRoute{
			From:     r.FromAccount,
			To:       r.ToAccount,
			Percent:  r.Percent,
			AutoVest: r.AutoVest,
		})
	}
	// hive_evaluator_transfer.cpp:447-453 — total percent across all
	// routes for from_account must be ≤ 100%.
	total, err := objects.SumWithdrawRoutePercentFrom(ctx.DB, r.FromAccount)
	if err != nil {
		return err
	}
	if total > 10000 {
		return fmt.Errorf("set_withdraw_vesting_route: total percent %d > 100%% across all routes", total)
	}
	return nil
}

// delegateVestingSharesEvaluator implements delegate_vesting_shares.
type delegateVestingSharesEvaluator struct{}

func (delegateVestingSharesEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagDelegateVestingShares {
		return fmt.Errorf("delegateVestingSharesEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	d := opArg.AsDelegateVestingShares()
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

	// Min-delegation + min-update enforcement. Mirrors hived
	// hive_evaluator_transfer.cpp:764-769:
	//
	//   min_delegation = (HF20+ ? fee/3 : fee*10) * vesting_share_price
	//   min_update     = (HF20+ ? fee/30: fee   ) * vesting_share_price
	//
	// where vesting_share_price = total_vesting_shares / total_vesting_fund_hive
	// (with the 1:1000 genesis fallback when either is zero). HF20 scaled
	// the fee meaning by 30×, so the thresholds were divided by ~10× to
	// keep the absolute minimum in line.
	minDelegation, minUpdate, vesterr := computeDelegationMinima(ctx)
	if vesterr == nil {
		isCreate := prevAmt == 0
		switch {
		case isCreate:
			if d.VestingShares.Amount < minDelegation {
				return fmt.Errorf("delegate_vesting_shares: minimum delegation is %d VESTS (got %d)",
					minDelegation, d.VestingShares.Amount)
			}
		case delta > 0:
			if delta < minUpdate {
				return fmt.Errorf("delegate_vesting_shares: minimum increase is %d VESTS (got %d)",
					minUpdate, delta)
			}
		case delta < 0:
			// Decreasing: delta < 0. Hived checks `delta >= min_update`
			// where delta is the POSITIVE difference (prev - new).
			absDelta := -delta
			if d.VestingShares.Amount > 0 {
				if absDelta < minUpdate {
					return fmt.Errorf("delegate_vesting_shares: minimum decrease is %d VESTS (got %d)",
						minUpdate, absDelta)
				}
				if d.VestingShares.Amount < minDelegation {
					return fmt.Errorf("delegate_vesting_shares: cannot reduce below min_delegation (%d) — either remove (set 0) or leave ≥ %d",
						minDelegation, minDelegation)
				}
			} else {
				// Setting to 0 — full removal. Hived requires prev > 0
				// (which is already implied by delta < 0 here).
			}
		}
	}

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
	// HF20+: both sides' effective_vests changed → refresh RC caps +
	// clip any RC delegations now exceeding the new max. Mirrors C++
	// rc_plugin.cpp post_apply on delegate_vesting_shares_operation,
	// which calls update_after_vest_change(delegator, true, true) and
	// update_after_vest_change(delegatee, true, true). Both sides need
	// the overflow check because (a) the delegator's DelegatedVestingShares
	// just rose (lowering their effective_vests), and (b) the delegatee's
	// ReceivedVestingShares may have fallen back to 0 (lowering their
	// effective_vests if they redelegated as RC).
	if ctx.Hardforks.Has(hardfork.HF20) {
		objects.HandleVestingWithdrawalRCImpact(ctx.DB, &delegator, ctx.HeadBlockTime)
		objects.HandleVestingWithdrawalRCImpact(ctx.DB, &delegatee, ctx.HeadBlockTime)
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

// computeDelegationMinima returns (min_delegation, min_update) in VESTS
// satoshi. Mirrors hived's HF20 reduction by 10× (fee meaning went up
// 30× at HF20, so the delegation thresholds compensate):
//
//	HF20+: min_delegation = fee/3   * price; min_update = fee/30 * price
//	pre-HF20: min_delegation = fee*10 * price; min_update = fee    * price
//
// `price` = total_vesting_shares / total_vesting_fund_hive (1:1000
// genesis fallback). Returns (0, 0, err) when the witness median fee
// is unknown — callers gracefully skip the check in that case.
func computeDelegationMinima(ctx *Context) (int64, int64, error) {
	ws, err := objects.GetWitnessSchedule(ctx.DB)
	if err != nil {
		return 0, 0, err
	}
	dgpo, err := objects.GetDGPO(ctx.DB)
	if err != nil {
		return 0, 0, err
	}
	fee := ws.MedianAccountCreationFee
	if fee <= 0 {
		return 0, 0, nil
	}
	var feeMin, feeUpd int64
	if ctx.Hardforks.Has(hardfork.HF20) {
		feeMin = fee / 3
		feeUpd = fee / 30
	} else {
		feeMin = fee * 10
		feeUpd = fee
	}
	// Convert HIVE→VESTS at current price = total_vesting_shares /
	// total_vesting_fund_hive. Use the 1:1000 genesis fallback when
	// either total is zero (matches createVestingImpl semantics).
	priceVests := dgpo.TotalVestingShares
	priceFund := dgpo.TotalVestingFundHive
	convert := func(hivePortion int64) int64 {
		if priceFund == 0 || priceVests == 0 {
			return hivePortion * 1000
		}
		// hivePortion * priceVests / priceFund via 128-bit math.
		hi, lo := bits.Mul64(uint64(hivePortion), uint64(priceVests))
		if hi >= uint64(priceFund) {
			return math.MaxInt64
		}
		q, _ := bits.Div64(hi, lo, uint64(priceFund))
		if q > uint64(math.MaxInt64) {
			return math.MaxInt64
		}
		return int64(q)
	}
	return convert(feeMin), convert(feeUpd), nil
}

func init() {
	register(op.TagWithdrawVesting, withdrawVestingEvaluator{})
	register(op.TagSetWithdrawVestingRoute, setWithdrawVestingRouteEvaluator{})
	register(op.TagDelegateVestingShares, delegateVestingSharesEvaluator{})
}
