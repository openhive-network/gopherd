package evaluator

import (
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// claimRewardBalanceEvaluator implements claim_reward_balance (HF17+),
// moving funds from the per-account reward balances into the spendable
// liquid + vesting balances. Each component (HIVE, HBD, VESTS) is
// validated against the account's pending reward of that type and clamped
// at zero.
type claimRewardBalanceEvaluator struct{}

func (claimRewardBalanceEvaluator) Apply(ctx *Context, opArg op.Op) error {
	o := opArg.Operation()
	cr, ok := o.(op.ClaimRewardBalance)
	if !ok {
		return fmt.Errorf("claimRewardBalanceEvaluator: unexpected op type %T", o)
	}

	if cr.RewardHive.Symbol.Num != asset.HiveSymbol {
		return fmt.Errorf("%w: reward_hive must be HIVE", ErrUnsupportedSymbol)
	}
	if cr.RewardHbd.Symbol.Num != asset.HbdSymbol {
		return fmt.Errorf("%w: reward_hbd must be HBD", ErrUnsupportedSymbol)
	}
	if cr.RewardVests.Symbol.Num != asset.VestsSymbol {
		return fmt.Errorf("%w: reward_vests must be VESTS", ErrUnsupportedSymbol)
	}
	if cr.RewardHive.Amount < 0 || cr.RewardHbd.Amount < 0 || cr.RewardVests.Amount < 0 {
		return ErrTransferAmountNotPos
	}

	a, err := objects.GetAccount(ctx.DB, cr.Account)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: account=%q", ErrAccountNotFound, cr.Account)
		}
		return err
	}
	if cr.RewardHive.Amount > a.RewardHive {
		return fmt.Errorf("%w: %q reward_hive %d, claim %d", ErrInsufficientBalance, cr.Account, a.RewardHive, cr.RewardHive.Amount)
	}
	if cr.RewardHbd.Amount > a.RewardHBD {
		return fmt.Errorf("%w: %q reward_hbd %d, claim %d", ErrInsufficientBalance, cr.Account, a.RewardHBD, cr.RewardHbd.Amount)
	}
	if cr.RewardVests.Amount > a.RewardVests {
		return fmt.Errorf("%w: %q reward_vests %d, claim %d", ErrInsufficientBalance, cr.Account, a.RewardVests, cr.RewardVests.Amount)
	}

	// HF20+: refresh RC manabar BEFORE any vesting change.
	if ctx.Hardforks.Has(hardfork.HF20) {
		objects.RegenerateRCMana(&a, ctx.HeadBlockTime)
	}
	a.RewardHive -= cr.RewardHive.Amount
	a.Balance += cr.RewardHive.Amount
	a.RewardHBD -= cr.RewardHbd.Amount
	objects.AdjustHBDBalance(ctx.DB, &a, ctx.HeadBlockTime, cr.RewardHbd.Amount)
	a.RewardVests -= cr.RewardVests.Amount
	a.VestingShares += cr.RewardVests.Amount
	// Refresh again so LastMaxRC tracks the post-claim cap.
	if ctx.Hardforks.Has(hardfork.HF20) {
		objects.RegenerateRCMana(&a, ctx.HeadBlockTime)
	}
	objects.PutAccount(ctx.DB, &a)
	// hive_evaluator_transfer.cpp:717 — propagate the new VESTS into the
	// witness-vote totals up the proxy chain.
	if cr.RewardVests.Amount != 0 {
		objects.AdjustProxiedWitnessVotes(ctx.DB, &a, cr.RewardVests.Amount)
	}
	return nil
}

func init() {
	register(op.TagClaimRewardBalance, claimRewardBalanceEvaluator{})
}
