package chain

import (
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// clearAccounts zeroes the liquid HIVE/HBD, savings, and VESTS of every
// account in `names`. Mirrors database::clear_accounts
// (libraries/chain/database.cpp:1461). Used at HF23 to drain the
// Steemit Inc. accounts and the Tron-aligned puppets identified during
// the Steem→Hive split.
//
// We can't fully model "vest balance to HIVE conversion at fund price"
// without running the inverse VESTS→HIVE on the dgpo's fund/shares; the
// total_vesting_shares decrement is unconditional in hived. We mirror
// the simplification: drop vesting_shares to zero and decrement the
// global pool by the same amount, leaving the fund_hive untouched.
// (Hived sells the VESTS at the fund price and credits the proceeds to
// the new owner; here the cleared accounts are losing the assets, so
// the proceeds direction doesn't apply.)
func clearAccounts(s *state.Session, names []string) {
	dgpo, _ := objects.GetDGPO(s)
	blockTime := dgpo.Time
	dgpoChanged := false
	for _, name := range names {
		a, err := objects.GetAccount(s, name)
		if err != nil {
			continue
		}
		// Snapshot supply deltas BEFORE adjusting (matches the snapshot
		// pattern in clear_null_account_balance).
		hbdSnap := a.HBDBalance
		hbdSavingsSnap := a.SavingsHBDBalance
		// Liquid HIVE / savings_HIVE: no interest hook, zero directly.
		if a.Balance > 0 {
			dgpo.CurrentSupply -= a.Balance
			a.Balance = 0
			dgpoChanged = true
		}
		if a.SavingsBalance > 0 {
			dgpo.CurrentSupply -= a.SavingsBalance
			a.SavingsBalance = 0
			dgpoChanged = true
		}
		// HBD / savings_HBD: route through the interest-accrual hooks
		// so any pending 30-day window mints to current_hbd_supply
		// before the negative delta lands. Mirrors the C++ pattern of
		// adjust_balance(account, -hbd_balance) at clear_accounts.
		if hbdSnap > 0 {
			if blockTime != 0 {
				objects.AdjustHBDBalance(s, &a, blockTime, -hbdSnap)
			} else {
				a.HBDBalance = 0
			}
			dgpo.CurrentHBDSupply -= hbdSnap
			dgpoChanged = true
		}
		if hbdSavingsSnap > 0 {
			if blockTime != 0 {
				objects.AdjustSavingsHBDBalance(s, &a, blockTime, -hbdSavingsSnap)
			} else {
				a.SavingsHBDBalance = 0
			}
			dgpo.CurrentHBDSupply -= hbdSavingsSnap
			dgpoChanged = true
		}
		if a.VestingShares > 0 {
			dgpo.TotalVestingShares -= a.VestingShares
			a.VestingShares = 0
			dgpoChanged = true
		}
		// Reward bucket
		if a.RewardHive > 0 {
			a.RewardHive = 0
		}
		if a.RewardHBD > 0 {
			a.RewardHBD = 0
		}
		if a.RewardVests > 0 {
			a.RewardVests = 0
		}
		objects.PutAccount(s, &a)
	}
	if dgpoChanged {
		objects.PutDGPO(s, &dgpo)
	}
}

// restoreAccounts is the HF24 counterpart to clearAccounts: a small set
// of accounts had their HF23 clear reverted because subsequent
// investigation determined they were not aligned with the Tron
// takeover. We can't actually un-burn the funds (we don't snapshot
// pre-clear balances), so this is effectively a no-op in our model
// beyond the structural acknowledgement.
//
// Hived's restore_accounts (database.cpp:1422) reads from a snapshot
// taken right before HF23 fired and re-credits each restored account
// with its pre-clear balances. Without that snapshot we'd need to
// rewind, which the Pebble-backed undo log doesn't keep this far back.
// The replay diverges here; flagged as a known limitation.
func restoreAccounts(s *state.Session, names []string) {
	// Intentional no-op — see comment above.
	_, _ = s, names
}

// consolidateTreasuryBalance moves all assets from the old treasury
// (steem.dao) into the new one (hive.fund) at HF24. Mirrors
// database::consolidate_treasury_balance (database.cpp:1267).
func consolidateTreasuryBalance(s *state.Session, blockTime uint32) {
	const (
		oldTreasuryName = "steem.dao"
		newTreasuryName = "hive.fund"
	)
	old, oerr := objects.GetAccount(s, oldTreasuryName)
	if oerr != nil {
		return
	}
	newAcct, nerr := objects.GetAccount(s, newTreasuryName)
	if nerr != nil {
		// Create the new treasury if absent.
		newAcct = objects.Account{
			Name:                 newTreasuryName,
			Created:              blockTime,
			VotingManaCurrent:    10000,
			VotingManaLastUpdate: blockTime,
		}
	}
	newAcct.Balance += old.Balance
	old.Balance = 0
	newAcct.SavingsBalance += old.SavingsBalance
	old.SavingsBalance = 0
	objects.AdjustHBDBalance(s, &newAcct, blockTime, old.HBDBalance)
	old.HBDBalance = 0
	objects.AdjustSavingsHBDBalance(s, &newAcct, blockTime, old.SavingsHBDBalance)
	old.SavingsHBDBalance = 0

	if old.VestingShares > 0 {
		// Hived liquifies the VESTS at the fund price and adds the HIVE
		// to the treasury. Mirror that with the dynamic vests-to-hive
		// rate.
		dgpo, _ := objects.GetDGPO(s)
		hiveValue := vestsToHive(old.VestingShares, &dgpo)
		newAcct.Balance += hiveValue
		dgpo.TotalVestingShares -= old.VestingShares
		dgpo.TotalVestingFundHive -= hiveValue
		objects.PutDGPO(s, &dgpo)
		old.VestingShares = 0
	}

	newAcct.RewardHive += old.RewardHive
	old.RewardHive = 0
	newAcct.RewardHBD += old.RewardHBD
	old.RewardHBD = 0
	newAcct.RewardVests += old.RewardVests
	old.RewardVests = 0

	objects.PutAccount(s, &old)
	objects.PutAccount(s, &newAcct)
}
