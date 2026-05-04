package main

import (
	"fmt"
	"os"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// getws is a small inspector for Go state snapshots.
//
//	getws <snapshot.bin>                               — prints DGPO + schedule globals
//	getws <snapshot.bin> account <name1> [name2 ...]   — prints balances for the named accounts
//	getws <snapshot.bin> witness <name1> [name2 ...]   — prints pow_worker / votes / VST
//	getws <snapshot.bin> feed                          — prints feed history medians
func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: getws <snapshot.bin> [account|witness|feed [name...]]")
		os.Exit(2)
	}
	db := state.OpenMem()
	if _, err := db.LoadSnapshot(os.Args[1]); err != nil {
		panic(err)
	}
	s := state.NewSession(db)

	if len(os.Args) == 2 {
		ws, err := objects.GetWitnessSchedule(s)
		if err != nil {
			panic(err)
		}
		dgpo, err := objects.GetDGPO(s)
		if err != nil {
			panic(err)
		}
		fmt.Printf("num_scheduled=%d max_voted=%d max_miner=%d max_runner=%d num_pow_witnesses=%d total_pow=%d head=%d\n",
			ws.NumScheduledWitnesses, ws.MaxVotedWitnesses, ws.MaxMinerWitnesses, ws.MaxRunnerWitnesses,
			dgpo.NumPowWitnesses, dgpo.TotalPow, dgpo.HeadBlockNumber)
		fmt.Printf("virtual_supply=%d current_supply=%d hbd_supply=%d vesting_shares=%d vesting_fund_hive=%d reward_fund_hive=%d\n",
			dgpo.VirtualSupply, dgpo.CurrentSupply, dgpo.CurrentHBDSupply,
			dgpo.TotalVestingShares, dgpo.TotalVestingFundHive, dgpo.TotalRewardFundHive)
		fmt.Printf("total_reward_shares2=%x\n", dgpo.TotalRewardShares2)
		return
	}

	switch os.Args[2] {
	case "active":
		ws, err := objects.GetWitnessSchedule(s)
		if err != nil {
			panic(err)
		}
		for _, w := range ws.CurrentShuffledWitnesses {
			fmt.Println(string(w))
		}
	case "account":
		for _, name := range os.Args[3:] {
			a, err := objects.GetAccount(s, name)
			if err != nil {
				fmt.Printf("%s: NOT FOUND (%v)\n", name, err)
				continue
			}
			fmt.Printf("%s: balance=%d hbd=%d vesting=%d savings=%d savings_hbd=%d reward_hbd=%d\n",
				name, a.Balance, a.HBDBalance, a.VestingShares,
				a.SavingsBalance, a.SavingsHBDBalance, a.RewardHBD)
		}
	case "witness":
		for _, name := range os.Args[3:] {
			w, err := objects.GetWitness(s, auth.AccountName(name))
			if err != nil {
				fmt.Printf("%s: NOT FOUND (%v)\n", name, err)
				continue
			}
			fmt.Printf("%s: pow_worker=%d votes=%d schedule=%v vst=%x rate=%d/%d last_update=%d\n",
				name, w.PowWorker, w.TotalVotesVests, w.Schedule, w.VirtualScheduledTime[:8],
				w.HBDExchangeRate.Base.Amount, w.HBDExchangeRate.Quote.Amount, w.LastHBDExchangeUpdate)
		}
	case "feed":
		fh, err := objects.GetFeedHistory(s)
		if err != nil {
			panic(err)
		}
		fmt.Printf("median=%d/%d\n",
			fh.CurrentMedianHistory.Base.Amount,
			fh.CurrentMedianHistory.Quote.Amount)
		fmt.Printf("history len=%d (last 10):\n", len(fh.PriceHistory))
		start := len(fh.PriceHistory) - 10
		if start < 0 {
			start = 0
		}
		for _, p := range fh.PriceHistory[start:] {
			fmt.Printf("  %d/%d\n", p.Base.Amount, p.Quote.Amount)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %q\n", os.Args[2])
		os.Exit(2)
	}
}
