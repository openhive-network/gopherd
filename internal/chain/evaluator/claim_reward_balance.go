package evaluator

import (
	"fmt"
	"math"
	"math/bits"
	"os"

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
	if opArg.Tag != op.TagClaimRewardBalance {
		return fmt.Errorf("claimRewardBalanceEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	cr := opArg.AsClaimRewardBalance()

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
	// Drift-localization trace: a claim_reward_balance op's amounts equal
	// hived's reward_* balances at this block (the op must match the chain),
	// so logging (claimed vs Go's a.Reward*) at each traced account's claim
	// pinpoints the FIRST claim where Go diverges — localizing a reward drift
	// to the cashout window before it, with NO hived node. Enable with
	// GHIVEDIFF=<account> (e.g. kennybll for the 21,539,933 drift).
	if vestTraceAccount(string(cr.Account)) {
		fmt.Fprintf(os.Stderr, "GHIVEDIFF CLAIM block=%d account=%s claim_vests=%d go_reward_vests=%d claim_hbd=%d go_reward_hbd=%d claim_hive=%d go_reward_hive=%d\n",
			ctx.BlockNum, cr.Account, cr.RewardVests.Amount, a.RewardVests, cr.RewardHbd.Amount, a.RewardHBD, cr.RewardHive.Amount, a.RewardHive)
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

	// hive_evaluator_transfer.cpp:650-655 — the HIVE backing the claimed
	// VESTS that must move from pending → main pool is proportional:
	//   if full claim: hive_to_move = a.RewardVestingHive
	//   else         : hive_to_move = claim_vests × a.RewardVestingHive / a.RewardVests
	var rewardVestingHiveToMove int64
	if cr.RewardVests.Amount == a.RewardVests {
		rewardVestingHiveToMove = a.RewardVestingHive
	} else if cr.RewardVests.Amount > 0 && a.RewardVests > 0 {
		hi, lo := bits.Mul64(uint64(cr.RewardVests.Amount), uint64(a.RewardVestingHive))
		d := uint64(a.RewardVests)
		if hi >= d {
			rewardVestingHiveToMove = math.MaxInt64
		} else {
			q, _ := bits.Div64(hi, lo, d)
			if q > math.MaxInt64 {
				rewardVestingHiveToMove = math.MaxInt64
			} else {
				rewardVestingHiveToMove = int64(q)
			}
		}
	}

	a.RewardVests -= cr.RewardVests.Amount
	a.RewardVestingHive -= rewardVestingHiveToMove
	// HF20+: regen the voting manabar at the PRE-credit effective-vesting
	// cap and bump it by the claimed vests, before the vests land
	// (hive_evaluator_transfer.cpp:698-703 runs util::update_manabar with
	// op_reward_vests ahead of the access_vesting() transfer).
	if ctx.Hardforks.Has(hardfork.HF20) {
		dgpoRef, _ := objects.GetDGPORef(ctx.DB)
		var dvPoolPct uint16
		if dgpoRef != nil {
			dvPoolPct = dgpoRef.DownvotePoolPercent
		}
		manaBefore, updBefore := a.VotingManaCurrent, a.VotingManaLastUpdate
		objects.UpdateVotingManabar(&a, ctx.HeadBlockTime, dvPoolPct, cr.RewardVests.Amount, ctx.BlockNum)
		if vestTraceAccount(string(cr.Account)) {
			ghiveDiff("GHIVEDIFF CLAIM-MANA block=%d acct=%s now=%d max=%d mana_before=%d upd_before=%d bump=%d mana_after=%d upd_after=%d\n",
				ctx.BlockNum, cr.Account, ctx.HeadBlockTime, a.EffectiveVestingSharesExclWithdraw(),
				manaBefore, updBefore, cr.RewardVests.Amount, a.VotingManaCurrent, a.VotingManaLastUpdate)
		}
	}
	a.VestingShares += cr.RewardVests.Amount
	// Refresh again so LastMaxRC tracks the post-claim cap (C++
	// update_account_after_vest_change).
	if ctx.Hardforks.Has(hardfork.HF20) {
		objects.RegenerateRCMana(&a, ctx.HeadBlockTime)
	}
	objects.PutAccount(ctx.DB, &a)

	// Move VESTS + HIVE backing from pending → main pool so the regular
	// (non-reward) vesting share price reflects the now-spendable balance.
	// Mirrors hive_evaluator_transfer.cpp:677-684.
	if cr.RewardVests.Amount != 0 || rewardVestingHiveToMove != 0 {
		objects.MutateDGPO(ctx.DB, func(p *objects.DynamicGlobalProperty) {
			p.TotalVestingShares += cr.RewardVests.Amount
			p.TotalVestingFundHive += rewardVestingHiveToMove
			p.PendingRewardedVestingShares -= cr.RewardVests.Amount
			p.PendingRewardedVestingHive -= rewardVestingHiveToMove
		})
	}

	// hive_evaluator_transfer.cpp:686 — propagate the new VESTS into the
	// witness-vote totals up the proxy chain.
	if cr.RewardVests.Amount != 0 {
		objects.AdjustProxiedWitnessVotes(ctx.DB, &a, cr.RewardVests.Amount)
	}
	return nil
}

func init() {
	register(op.TagClaimRewardBalance, claimRewardBalanceEvaluator{})
}
