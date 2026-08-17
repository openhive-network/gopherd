package main

// validate-state walks the chain state and verifies the canonical
// hived invariants:
//
//   sum_of_account_balances + treasury + reward_fund_hive == current_supply
//   sum_of_account_hbd + treasury_hbd + reward_fund_hbd  == current_hbd_supply
//   sum_of_account_vesting_shares                         == total_vesting_shares
//
// hived runs validate_invariants() at every block during dev builds. We
// can run it against a snapshot to spot accounting bugs.
//
// Usage: ghive-tools validate-state --data-dir=PATH

import (
	"flag"
	"fmt"
	"os"

	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

func cmdValidateState(args []string) int {
	fs := flag.NewFlagSet("validate-state", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "Pebble data dir (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "validate-state: --data-dir required")
		return 2
	}

	db, err := state.Open(*dataDir, 256<<20)
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate-state: %v\n", err)
		return 1
	}
	defer db.Close()

	s := state.NewSession(db)
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate-state: dgpo: %v\n", err)
		return 1
	}

	var sumBalance, sumHBD, sumVesting, sumSavings, sumSavingsHBD int64
	var sumRewardHive, sumRewardHBD, sumRewardVests int64
	accounts := 0

	_ = s.Range(objects.AccountTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			a, err := objects.ReadAccount(value)
			if err != nil {
				return true, nil
			}
			sumBalance += a.Balance
			sumHBD += a.HBDBalance
			sumVesting += a.VestingShares
			sumSavings += a.SavingsBalance
			sumSavingsHBD += a.SavingsHBDBalance
			sumRewardHive += a.RewardHive
			sumRewardHBD += a.RewardHBD
			sumRewardVests += a.RewardVests
			accounts++
			return true, nil
		})

	// HIVE supply: account balances + reward_hive + savings + reward_fund_hive
	// (the dgpo TVFH backs vests separately). Mainnet adds reward_fund balance too.
	rewardFundBalance := int64(0)
	if rf, err := objects.GetRewardFund(s, objects.PostRewardFundName); err == nil {
		rewardFundBalance = rf.RewardBalance
	}

	hiveTotal := sumBalance + sumRewardHive + sumSavings + dgpo.TotalVestingFundHive +
		dgpo.TotalRewardFundHive + rewardFundBalance
	hbdTotal := sumHBD + sumRewardHBD + sumSavingsHBD

	fmt.Printf("--- ghive state validation ---\n")
	fmt.Printf("accounts:                     %d\n", accounts)
	fmt.Printf("dgpo.head_block_number:       %d\n", dgpo.HeadBlockNumber)
	fmt.Printf("\n[HIVE]\n")
	fmt.Printf("sum balance:                  %d\n", sumBalance)
	fmt.Printf("sum savings:                  %d\n", sumSavings)
	fmt.Printf("sum reward_hive:              %d\n", sumRewardHive)
	fmt.Printf("dgpo.total_vesting_fund_hive: %d\n", dgpo.TotalVestingFundHive)
	fmt.Printf("dgpo.total_reward_fund_hive:  %d\n", dgpo.TotalRewardFundHive)
	fmt.Printf("post fund.reward_balance:     %d\n", rewardFundBalance)
	fmt.Printf("computed hive_total:          %d\n", hiveTotal)
	fmt.Printf("dgpo.current_supply:          %d\n", dgpo.CurrentSupply)
	hiveDiff := dgpo.CurrentSupply - hiveTotal
	if hiveDiff != 0 {
		fmt.Printf("DIFF (current_supply - sum):  %d (%+.4f%%)\n",
			hiveDiff, 100*float64(hiveDiff)/float64(dgpo.CurrentSupply+1))
	} else {
		fmt.Println("HIVE invariant: OK")
	}

	fmt.Printf("\n[HBD]\n")
	fmt.Printf("sum hbd:                      %d\n", sumHBD)
	fmt.Printf("sum savings_hbd:              %d\n", sumSavingsHBD)
	fmt.Printf("sum reward_hbd:               %d\n", sumRewardHBD)
	fmt.Printf("computed hbd_total:           %d\n", hbdTotal)
	fmt.Printf("dgpo.current_hbd_supply:      %d\n", dgpo.CurrentHBDSupply)
	hbdDiff := dgpo.CurrentHBDSupply - hbdTotal
	if hbdDiff != 0 {
		fmt.Printf("DIFF (hbd_supply - sum):      %d (%+.4f%%)\n",
			hbdDiff, 100*float64(hbdDiff)/float64(dgpo.CurrentHBDSupply+1))
	} else {
		fmt.Println("HBD invariant: OK")
	}

	fmt.Printf("\n[VESTS]\n")
	fmt.Printf("sum vesting_shares:           %d\n", sumVesting)
	fmt.Printf("sum reward_vests:             %d\n", sumRewardVests)
	fmt.Printf("dgpo.total_vesting_shares:    %d\n", dgpo.TotalVestingShares)
	vestDiff := dgpo.TotalVestingShares - sumVesting
	if vestDiff != 0 {
		fmt.Printf("DIFF (total_shares - sum):    %d (%+.4f%%)\n",
			vestDiff, 100*float64(vestDiff)/float64(dgpo.TotalVestingShares+1))
	} else {
		fmt.Println("VESTS invariant: OK")
	}

	if hiveDiff != 0 || hbdDiff != 0 || vestDiff != 0 {
		return 1
	}
	return 0
}
