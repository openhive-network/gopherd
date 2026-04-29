package chain

import (
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_VESTING_WITHDRAW_INTERVAL_SECONDS — 1 week between payouts.
const vestingWithdrawIntervalSec uint32 = 60 * 60 * 24 * 7

// hive_to_vests_satoshi_ratio mirrors the genesis 1:1000 conversion price
// CreateVesting uses: 1 HIVE-satoshi (precision 3) → 1000 VESTS-satoshi
// (precision 6). Reverse direction (VESTS → HIVE) divides by 1000.
const vestsToHiveSatoshiRatio int64 = 1000

// processVestingWithdrawals mirrors database::process_vesting_withdrawals
// (libraries/chain/database_vesting.cpp:111).
//
// Each block, every account whose next_vesting_withdrawal time has passed
// gets a chunk of its scheduled VESTS converted back to HIVE (or routed
// to other accounts at the configured percentages). The schedule advances
// by HIVE_VESTING_WITHDRAW_INTERVAL_SECONDS until ToWithdraw is exhausted
// (or the account has run out of VESTS to back the schedule).
//
// We iterate the VestingWithdrawalSchedule secondary index in time order
// and stop at the first entry whose time is in the future. Each processed
// entry updates the account, the dgpo totals, and the schedule index.
//
// The full hived flow also generates a fill_vesting_withdraw virtual op
// (for history) and applies HF20+ RC mana regeneration / HF24+ delayed
// voting tracking; those land with the corresponding subsystems.
func processVestingWithdrawals(s *state.Session, blockTime uint32) {
	type pending struct {
		nextWithdrawal uint32
		account        string
	}
	var ready []pending

	// Snapshot ready entries first to avoid invalidating the iterator
	// while we mutate the same table.
	_ = s.Range(objects.VestingWithdrawalScheduleTable, state.PrimaryIndex, nil,
		func(body, _ []byte) (bool, error) {
			if len(body) < 4+state.AccountNameKeyLen {
				return true, nil
			}
			t := uint32(body[0])<<24 | uint32(body[1])<<16 | uint32(body[2])<<8 | uint32(body[3])
			if t > blockTime {
				return false, nil
			}
			name := body[4 : 4+state.AccountNameKeyLen]
			// Strip trailing NULs (account_name is NUL-padded to 16 bytes).
			n := state.AccountNameKeyLen
			for n > 0 && name[n-1] == 0 {
				n--
			}
			ready = append(ready, pending{nextWithdrawal: t, account: string(name[:n])})
			return true, nil
		})

	for _, p := range ready {
		processOneWithdrawal(s, p.account, p.nextWithdrawal)
	}
}

// processOneWithdrawal pays out the next chunk of an account's scheduled
// vesting withdrawal — convert min(rate, remaining) VESTS to HIVE, credit
// from_account.balance (and any routed accounts), update DGPO totals,
// advance or clear the schedule.
func processOneWithdrawal(s *state.Session, accountName string, scheduledTime uint32) {
	objects.DeleteVestingWithdrawalSchedule(s, scheduledTime, accountName)

	a, err := objects.GetAccount(s, accountName)
	if err != nil {
		return
	}
	if a.VestingWithdrawRate == 0 || a.NextVestingWithdrawal == 0 {
		return
	}

	remaining := a.ToWithdraw - a.Withdrawn
	if remaining <= 0 {
		a.VestingWithdrawRate = 0
		a.NextVestingWithdrawal = 0
		a.ToWithdraw = 0
		a.Withdrawn = 0
		objects.PutAccount(s, &a)
		return
	}
	toWithdraw := a.VestingWithdrawRate
	if toWithdraw > remaining {
		toWithdraw = remaining
	}
	if toWithdraw > a.VestingShares {
		toWithdraw = a.VestingShares
	}
	if toWithdraw <= 0 {
		a.VestingWithdrawRate = 0
		a.NextVestingWithdrawal = 0
		a.ToWithdraw = 0
		a.Withdrawn = 0
		objects.PutAccount(s, &a)
		return
	}

	// Routes (set_withdraw_vesting_route). For each route, divert
	// (percent/HIVE_100_PERCENT) of toWithdraw — first as VESTS for
	// auto_vest=true entries (VESTS stay in the vesting pool), then as
	// HIVE for auto_vest=false entries (price-converted via the genesis
	// 1:1000 ratio). The remainder is paid to the account itself in HIVE.
	var depositedVests int64
	depositedVests += processWithdrawRoutes(s, accountName, toWithdraw, true)
	depositedVests += processWithdrawRoutes(s, accountName, toWithdraw, false)

	// Re-read account in case routing modified it (specifically, if a
	// route loops back to the source account or shares the dgpo).
	a, err = objects.GetAccount(s, accountName)
	if err != nil {
		return
	}

	toConvertToHive := toWithdraw - depositedVests
	convertedHive := toConvertToHive / vestsToHiveSatoshiRatio

	a.VestingShares -= toWithdraw
	a.Balance += convertedHive
	a.Withdrawn += toWithdraw

	if a.Withdrawn >= a.ToWithdraw || a.VestingShares == 0 {
		a.VestingWithdrawRate = 0
		a.NextVestingWithdrawal = 0
		a.ToWithdraw = 0
		a.Withdrawn = 0
	} else {
		a.NextVestingWithdrawal = scheduledTime + vestingWithdrawIntervalSec
		objects.PutVestingWithdrawalSchedule(s, a.NextVestingWithdrawal, accountName)
	}
	objects.PutAccount(s, &a)

	dgpo, err := objects.GetDGPO(s)
	if err == nil {
		dgpo.TotalVestingFundHive -= convertedHive
		dgpo.TotalVestingShares -= toConvertToHive
		objects.PutDGPO(s, &dgpo)
	}
}

// processWithdrawRoutes handles the set_withdraw_vesting_route diversions
// for one account's withdrawal chunk. autoVest controls which pass we're
// running — VESTS routes first (preserve precision), HIVE routes second.
// Returns the total VESTS-satoshi diverted in this pass.
func processWithdrawRoutes(s *state.Session, fromAccount string, toWithdraw int64, autoVest bool) int64 {
	var deposited int64
	prefix := state.AppendAccountName(nil, fromAccount)
	_ = s.Range(objects.WithdrawRouteTable, state.PrimaryIndex, prefix,
		func(_, value []byte) (bool, error) {
			route, rerr := objects.ReadWithdrawRoute(value)
			if rerr != nil {
				return true, nil
			}
			if route.AutoVest != autoVest {
				return true, nil
			}
			toDeposit := toWithdraw * int64(route.Percent) / 10000 // HIVE_100_PERCENT = 10000
			if toDeposit <= 0 {
				return true, nil
			}
			to, gerr := objects.GetAccount(s, route.To)
			if gerr != nil {
				return true, nil
			}
			if autoVest {
				to.VestingShares += toDeposit
			} else {
				to.Balance += toDeposit / vestsToHiveSatoshiRatio
				dgpo, derr := objects.GetDGPO(s)
				if derr == nil {
					dgpo.TotalVestingFundHive -= toDeposit / vestsToHiveSatoshiRatio
					dgpo.TotalVestingShares -= toDeposit
					objects.PutDGPO(s, &dgpo)
				}
			}
			objects.PutAccount(s, &to)
			deposited += toDeposit
			return true, nil
		})
	return deposited
}
