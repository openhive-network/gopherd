package main

import (
	"fmt"
	"os"
	"strconv"

	"hived-go/internal/blocklog"
	"hived-go/internal/chain"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
)

// Usage: dump_account <block_log> <stop_block> <account>
// Replays Go to <stop_block>, dumps the account's full balance + recent
// posts (with rshares / reward_weight / cashout state).
func main() {
	logPath := os.Args[1]
	stop, _ := strconv.Atoi(os.Args[2])
	acct := os.Args[3]

	stateDB := state.OpenMem()
	if err := genesis.Apply(stateDB); err != nil {
		_ = err
	}
	cdb := chain.New(stateDB)

	r, err := blocklog.Open(logPath)
	if err != nil {
		panic(err)
	}
	defer r.Close()

	for n := uint32(1); n <= uint32(stop); n++ {
		raw, err := r.Block(n)
		if err != nil {
			continue
		}
		_, err = cdb.ApplyBlock(raw, n, chain.ApplyOptions{
			SkipTransactionSignatures: true,
			SkipBlockSignature:        true,
			TolerateFailingOps:        true,
		}, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "apply block %d: %v\n", n, err)
		}
	}

	sess := state.NewSession(stateDB)
	dgpo, _ := objects.GetDGPO(sess)
	fmt.Printf("=== dgpo at %d ===\n", stop)
	fmt.Printf("supply=%d virtual_supply=%d hbd_supply=%d fund=%d vfund=%d vshares=%d\n",
		dgpo.CurrentSupply, dgpo.VirtualSupply, dgpo.CurrentHBDSupply,
		dgpo.TotalRewardFundHive, dgpo.TotalVestingFundHive, dgpo.TotalVestingShares)
	fmt.Printf("hbd_print_rate=%d (basis points)\n", dgpo.HBDPrintRate)

	a, err := objects.GetAccount(sess, auth.AccountName(acct))
	if err != nil {
		fmt.Printf("account %s not found: %v\n", acct, err)
		return
	}
	fmt.Printf("\n=== account %s ===\n", acct)
	fmt.Printf("balance=%d hbd=%d savings=%d savings_hbd=%d\n",
		a.Balance, a.HBDBalance, a.SavingsBalance, a.SavingsHBDBalance)
	fmt.Printf("vesting_shares=%d delegated=%d received=%d\n",
		a.VestingShares, a.DelegatedVestingShares, a.ReceivedVestingShares)
	fmt.Printf("reward_hive=%d reward_hbd=%d reward_vests=%d\n",
		a.RewardHive, a.RewardHBD, a.RewardVests)
	fmt.Printf("post_bandwidth=%d post_count=%d\n",
		a.PostBandwidth, a.PostCount)
	fmt.Printf("last_root_post=%d last_post=%d\n", a.LastRootPost, a.LastPost)
	fmt.Printf("voting_mana_current=%d voting_mana_last_update=%d\n",
		a.VotingManaCurrent, a.VotingManaLastUpdate)

	fmt.Printf("\n=== %s posts ===\n", acct)
	postCount := 0
	_ = objects.RangeAllComments(sess, func(c *objects.Comment) (bool, error) {
		if string(c.Author) != acct {
			return true, nil
		}
		postCount++
		fmt.Printf("[%d] permlink=%s parent=%q\n", postCount, c.Permlink, c.ParentAuthor)
		fmt.Printf("    net_rshares=%d abs_rshares=%d vote_rshares=%d\n",
			c.NetRshares, c.AbsRshares, c.VoteRshares)
		fmt.Printf("    reward_weight=%d cashout_time=%d max_cashout=%d\n",
			c.RewardWeight, c.CashoutTime, c.MaxCashoutTime)
		fmt.Printf("    was_paid=%v last_payout=%d max_payout=%d\n",
			c.WasPaid, c.LastPayout, c.MaxAcceptedPayout)
		fmt.Printf("    children_abs_rshares=%d total_vote_weight=%d\n",
			c.ChildrenAbsRshares, c.TotalVoteWeight)
		return true, nil
	})
	fmt.Printf("\nTotal %s posts: %d\n", acct, postCount)
}
