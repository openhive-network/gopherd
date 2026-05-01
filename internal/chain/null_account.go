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

	// Track totals to subtract from supply at the end (in HIVE/HBD).
	totalHIVE := a.Balance + a.SavingsBalance + a.RewardHive
	totalHBD := a.HBDBalance + a.SavingsHBDBalance + a.RewardHBD

	// Liquid + savings + rewards: zero in place.
	a.Balance = 0
	a.HBDBalance = 0
	a.SavingsBalance = 0
	a.SavingsHBDBalance = 0
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
			if vestsToHive > 0 {
				dgpo.CurrentSupply -= vestsToHive
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
		if totalHIVE > 0 {
			dgpo.CurrentSupply -= totalHIVE
		}
		if totalHBD > 0 {
			dgpo.CurrentHBDSupply -= totalHBD
		}
		objects.PutDGPO(s, &dgpo)
	}
}

