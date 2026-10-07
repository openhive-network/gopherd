package chain

import (
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// nullAccountName is the canonical burn address. Funds (HIVE / HBD /
// VESTS / savings / rewards) that arrive here are zeroed every block
// post-HF14, mirroring database::clear_null_account_balance
// (database.cpp:1162).
const nullAccountName = "null"

// clearNullAccountBalance is the per-block hook that drains the
// "null" account's balances and burns the corresponding chain-wide
// supply totals.
//
// Pre-HF14, no-op (matches HIVE_HARDFORK_0_14__327 gate).
func clearNullAccountBalance(s *state.Session, blockNum uint32, d *Database) {
	hf := hardfork.SetForBlock(blockNum)
	if !hf.Has(hardfork.HF14) {
		return
	}

	a, err := objects.GetAccount(s, nullAccountName)
	if err != nil {
		return // null account doesn't exist yet
	}

	// Snapshot totals pre-interest (mirrors C++ collect_account_total_balance
	// at database.cpp:1167-1172). The supply decrement at the end uses
	// these snapshotted values; any interest accrued by AdjustHBDBalance /
	// AdjustSavingsHBDBalance below mints to current_hbd_supply BEFORE
	// the snapshot subtracts, so net supply change is `-(X - I)`.
	// RewardVestingHive is the HIVE backing of pending reward vests — C++
	// includes it via account.get_vest_rewards_as_hive() in the total_hive
	// sum so it leaves current_supply alongside the regular HIVE balances.
	totalHIVE := a.Balance + a.SavingsBalance + a.RewardHive + a.RewardVestingHive
	totalHBD := a.HBDBalance + a.SavingsHBDBalance + a.RewardHBD

	// database.cpp:1142-1159 collect_account_total_balance + :1192-1203: when
	// nothing is held the sweep is skipped entirely; otherwise
	// clear_null_account_balance is pushed FIRST (pre-push) listing, in this
	// order and only when non-zero, the HIVE (liquid + savings + rewards +
	// the HIVE value of all vests), the VESTS (vesting + reward vests) and
	// the HBD (liquid + savings + rewards) about to be burned.
	vopHiveTotal := totalHIVE
	vopVestsTotal := a.VestingShares + a.RewardVests
	if a.VestingShares > 0 {
		if dgpoV, derr := objects.GetDGPO(s); derr == nil && dgpoV.TotalVestingShares > 0 {
			vopHiveTotal += mulDiv(a.VestingShares, dgpoV.TotalVestingFundHive, dgpoV.TotalVestingShares)
		}
	}
	if vopHiveTotal == 0 && vopVestsTotal == 0 && totalHBD == 0 {
		return
	}
	if d != nil {
		var cleared []asset.Asset
		if vopHiveTotal > 0 {
			cleared = append(cleared, vopHive(vopHiveTotal))
		}
		if vopVestsTotal > 0 {
			cleared = append(cleared, vopVests(vopVestsTotal))
		}
		if totalHBD > 0 {
			cleared = append(cleared, vopHbd(totalHBD))
		}
		d.pushVOp(op.ClearNullAccountBalance{TotalCleared: cleared})
	}

	// Mirror C++ database.cpp:1186-1204 — call adjust_balance /
	// adjust_savings_balance with the negated current balance so the
	// interest accrual hook fires before the delta lands. The
	// post-adjust account retains the just-minted interest as a residual,
	// which gets cleared on the next round.
	blockTime := uint32(0)
	if dgpoRef, ok := objects.GetDGPORef(s); ok {
		blockTime = dgpoRef.Time
	}
	if a.HBDBalance > 0 && blockTime != 0 {
		objects.AdjustHBDBalance(s, &a, blockTime, -a.HBDBalance)
	} else {
		a.HBDBalance = 0
	}
	if a.SavingsHBDBalance > 0 && blockTime != 0 {
		objects.AdjustSavingsHBDBalance(s, &a, blockTime, -a.SavingsHBDBalance)
	} else {
		a.SavingsHBDBalance = 0
	}

	// Liquid HIVE + savings HIVE + reward balances have no interest
	// hook in C++ adjust_balance — they zero directly.
	a.Balance = 0
	a.SavingsBalance = 0
	a.RewardHive = 0
	a.RewardHBD = 0

	// Vesting: convert the vests back to underlying HIVE for the supply
	// adjustment, then zero. The HIVE released from the vesting fund
	// must also leave the supply since the null account "burns" it.
	if a.VestingShares > 0 {
		dgpo, err := objects.GetDGPO(s)
		if err == nil && dgpo.TotalVestingShares > 0 {
			// vesting_shares_hive_value = vesting_shares * fund / shares
			vestsToHive := mulDiv(a.VestingShares, dgpo.TotalVestingFundHive,
				dgpo.TotalVestingShares)
			dgpo.TotalVestingShares -= a.VestingShares
			dgpo.TotalVestingFundHive -= vestsToHive
			// C++ folds vesting_hive_value into the final total_hive
			// `adjust_supply(-total_hive)`; we keep the vesting deduction
			// here, but apply it via adjustSupplyHive so virtual_supply
			// shrinks in step with current_supply (matches what C++
			// achieves through one combined adjust_supply call).
			if vestsToHive > 0 {
				adjustSupplyHive(s, &dgpo, -vestsToHive)
			}
			objects.PutDGPO(s, &dgpo)
		}
		a.VestingShares = 0
		a.SumDelayedVotes = 0
		a.DelayedVotes = nil
	}

	// Mirror C++ database.cpp:1239-1253: when a reward-pending account is
	// drained, decrement both pending-pool counters by the per-account
	// reward_vests / reward_vesting_hive contribution, and zero both
	// account-side fields. Without this the dgpo pending counters drift
	// upward by the drained amount on every null-clear.
	if a.RewardVests > 0 || a.RewardVestingHive > 0 {
		if dgpo, err := objects.GetDGPO(s); err == nil {
			if a.RewardVests > 0 {
				dgpo.PendingRewardedVestingShares -= a.RewardVests
			}
			if a.RewardVestingHive > 0 {
				dgpo.PendingRewardedVestingHive -= a.RewardVestingHive
			}
			objects.PutDGPO(s, &dgpo)
		}
		a.RewardVests = 0
		a.RewardVestingHive = 0
	}

	objects.PutAccount(s, &a)

	if dgpo, err := objects.GetDGPO(s); err == nil {
		// Mirror C++ clear_null_account_balance (database.cpp:1258-1262):
		// adjust_supply(-total_hive) then adjust_supply(-total_hbd). The
		// HBD branch does a full virtual_supply recompute — without it,
		// Go's intra-block VS lags C++'s by `total_hbd × median` until
		// the next end-of-block updateVirtualSupply.
		if totalHIVE > 0 {
			adjustSupplyHive(s, &dgpo, -totalHIVE)
		}
		if totalHBD > 0 {
			adjustSupplyHbd(s, &dgpo, -totalHBD)
		}
		objects.PutDGPO(s, &dgpo)
	}
}
