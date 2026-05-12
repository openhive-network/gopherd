package chain

import (
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
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
func clearNullAccountBalance(s *state.Session, blockNum uint32) {
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
	totalHIVE := a.Balance + a.SavingsBalance + a.RewardHive
	totalHBD := a.HBDBalance + a.SavingsHBDBalance + a.RewardHBD

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

	if a.RewardVests > 0 {
		// pending_rewarded_vesting_{shares,hive} accounting omitted —
		// we don't track those globals yet.
		a.RewardVests = 0
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

