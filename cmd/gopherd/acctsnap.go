package main

import (
	"fmt"
	"os"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// Usage: acctsnap <account_store.bin> <account> [account...]
// Reads a snapshot's account_store.bin directly (no cold tier needed) and
// prints the named accounts' full records — for diffing checkpoint state.
func cmdStateAccount(args []string) int {
	os.Args = append([]string{"gopherd-state-account"}, args...)

	f, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	boot := state.NewSession(state.OpenMem())
	st := objects.EnsureAccountStore(boot)
	if err := st.UnmarshalSnapshot(f); err != nil {
		panic(err)
	}
	if len(os.Args) >= 3 && os.Args[2] == "--sumhbd" {
		var liquid, savings, reward, total int64
		var n int
		var topName string
		var topHBD int64
		_ = st.RangePrimary(func(a *objects.Account) (bool, error) {
			liquid += a.HBDBalance
			savings += a.SavingsHBDBalance
			reward += a.RewardHBD
			total += a.HBDBalance + a.SavingsHBDBalance + a.RewardHBD
			n++
			if a.HBDBalance > topHBD {
				topHBD = a.HBDBalance
				topName = string(a.Name)
			}
			return true, nil
		})
		fmt.Printf("accounts=%d\n liquid_hbd=%d\n savings_hbd=%d\n reward_hbd=%d\n TOTAL(liquid+savings+reward)=%d\n top_liquid=%s(%d)\n", n, liquid, savings, reward, total, topName, topHBD)
		return 0
	}
	for _, name := range os.Args[2:] {
		a, ok := st.Get(auth.AccountName(name))
		if !ok {
			fmt.Printf("%s: NOT FOUND\n", name)
			continue
		}
		fmt.Printf("%s: %+v\n\n", name, a)
	}
	return 0
}
