package main

import (
	"fmt"
	"os"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
	snap "hived-go/internal/state/snapshot"
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
	if info, err := os.Stat(os.Args[1]); err == nil && info.IsDir() {
		boot := state.NewSession(db)
		objects.EnsureWitnessStore(boot)
		objects.EnsureWitnessVoteStore(boot)
		objects.EnsureAccountStore(boot)
		objects.EnsureCommentStore(boot)
		objects.EnsureCommentVoteStore(boot)
		objects.EnsureBlockSummaryStore(boot)
		objects.EnsureCommentCashoutScheduleStore(boot)
		objects.EnsureConvertRequestStore(boot)
		objects.EnsureDGPOStore(boot)
		objects.EnsureFutureWitnessScheduleStore(boot)
		objects.EnsureLimitOrderStore(boot)
		objects.EnsureTransactionStore(boot)
		objects.EnsureVestingWithdrawalScheduleStore(boot)
		objects.EnsureWitnessScheduleStore(boot)
		if _, err := snap.Load(db, os.Args[1]); err != nil {
			panic(err)
		}
	} else if _, err := db.LoadSnapshot(os.Args[1]); err != nil {
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
		fmt.Printf("num_scheduled=%d max_voted=%d max_miner=%d max_runner=%d num_pow_witnesses=%d total_pow=%d head=%d current_aslot=%d current_witness=%q\n",
			ws.NumScheduledWitnesses, ws.MaxVotedWitnesses, ws.MaxMinerWitnesses, ws.MaxRunnerWitnesses,
			dgpo.NumPowWitnesses, dgpo.TotalPow, dgpo.HeadBlockNumber, dgpo.CurrentAslot, dgpo.CurrentWitness)
		fmt.Printf("current_virtual_time=%x next_shuffle_block=%d witness_pay_norm=%d\n",
			ws.CurrentVirtualTime, ws.NextShuffleBlockNum, ws.WitnessPayNormalizationFactor)
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
			fmt.Printf("  mana: voting_current=%d voting_last_update=%d downvote_current=%d downvote_last_update=%d\n",
				a.VotingManaCurrent, a.VotingManaLastUpdate, a.DownvoteManaCurrent, a.DownvoteManaLastUpdate)
			fmt.Printf("  vests: delegated=%d received=%d to_withdraw=%d withdrawn=%d rate=%d reward_vests=%d reward_vesting_hive=%d\n",
				a.DelegatedVestingShares, a.ReceivedVestingShares, a.ToWithdraw, a.Withdrawn, a.VestingWithdrawRate, a.RewardVests, a.RewardVestingHive)
			if aa, err := objects.GetAccountAuthority(s, auth.AccountName(name)); err == nil {
				fmt.Printf("  authority: owner=%d-keys/threshold=%d active=%d-keys/threshold=%d posting=%d-keys/threshold=%d\n",
					len(aa.Owner.KeyAuths), aa.Owner.WeightThreshold,
					len(aa.Active.KeyAuths), aa.Active.WeightThreshold,
					len(aa.Posting.KeyAuths), aa.Posting.WeightThreshold)
			} else {
				fmt.Printf("  authority: MISSING (%v)\n", err)
			}
		}
	case "witness":
		for _, name := range os.Args[3:] {
			w, err := objects.GetWitness(s, auth.AccountName(name))
			if err != nil {
				fmt.Printf("%s: NOT FOUND (%v)\n", name, err)
				continue
			}
			fmt.Printf("%s: pow_worker=%d votes=%d schedule=%v rate=%d/%d created=%d last_confirmed=%d total_missed=%d\n",
				name, w.PowWorker, w.TotalVotesVests, w.Schedule,
				w.HBDExchangeRate.Base.Amount, w.HBDExchangeRate.Quote.Amount,
				w.Created, w.LastConfirmedBlockNum, w.TotalMissedBlocks)
			fmt.Printf("  vst                 =%x\n", w.VirtualScheduledTime)
			fmt.Printf("  virtual_last_update =%x\n", w.VirtualLastUpdate)
			fmt.Printf("  virtual_position    =%x\n", w.VirtualPosition)
		}
	case "feed":
		fh, err := objects.GetFeedHistory(s)
		if err != nil {
			panic(err)
		}
		fmt.Printf("median=%d/%d\n",
			fh.CurrentMedianHistory.Base.Amount,
			fh.CurrentMedianHistory.Quote.Amount)
		fmt.Printf("history len=%d (full):\n", len(fh.PriceHistory))
		for i, p := range fh.PriceHistory {
			fmt.Printf("  pos[%3d]: %d/%d\n", i, p.Base.Amount, p.Quote.Amount)
		}
	case "comment-votes":
		// usage: getws <snap> comment-votes <author> <permlink>
		author := auth.AccountName(os.Args[3])
		permlink := os.Args[4]
		c, err := objects.GetComment(s, author, permlink)
		if err != nil {
			fmt.Printf("comment NOT FOUND: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("comment author=%s permlink=%q id=%d net_rshares=%d total_vote_weight=%d\n",
			author, permlink, c.ID, c.NetRshares, c.TotalVoteWeight)
		var totalW uint64
		var n int
		prefix := objects.CommentVoteByCommentPrefix(author, permlink)
		_ = s.Range(objects.CommentVoteTable, objects.CommentVoteByCommentIndex, prefix,
			func(body, _ []byte) (bool, error) {
				if len(body) < state.AccountNameKeyLen {
					return true, nil
				}
				vb := body[len(body)-state.AccountNameKeyLen:]
				j := state.AccountNameKeyLen
				for j > 0 && vb[j-1] == 0 {
					j--
				}
				voter := auth.AccountName(string(vb[:j]))
				cv, gerr := objects.GetCommentVote(s, voter, author, permlink)
				if gerr != nil {
					return true, nil
				}
				fmt.Printf("  voter=%s rshares=%d weight=%d curation_weight=%d last_update=%d num_changes=%d\n",
					voter, cv.Rshares, cv.Weight, cv.CurationWeight, cv.LastUpdate, cv.NumChanges)
				totalW += cv.CurationWeight
				n++
				return true, nil
			})
		fmt.Printf("voter_count=%d sum_curation_weight=%d (vs comment.total_vote_weight=%d) diff=%d\n",
			n, totalW, c.TotalVoteWeight, int64(c.TotalVoteWeight)-int64(totalW))
	case "pending-cashouts":
		// Iterate the cashout schedule, filter by author, dump full comment
		// state for each pending entry.
		target := os.Args[3]
		_ = s.Range(objects.CommentCashoutScheduleTable, state.PrimaryIndex, nil,
			func(key, body []byte) (bool, error) {
				if len(key) < 12 {
					return true, nil
				}
				cashoutTime := uint32(key[0])<<24 | uint32(key[1])<<16 | uint32(key[2])<<8 | uint32(key[3])
				author, permlink := objects.DecodeCommentCashoutScheduleValue(body)
				if string(author) != target {
					return true, nil
				}
				c, err := objects.GetComment(s, author, permlink)
				if err != nil {
					fmt.Printf("schedule_time=%d author=%s permlink=%q ERR=%v\n", cashoutTime, author, permlink, err)
					return true, nil
				}
				fmt.Printf("schedule_time=%d author=%s permlink=%q id=%d net_rshares=%d total_vote_weight=%d percent_hbd=%d max_payout=%d allow_curation=%v cashout_time=%d last_payout=%d was_paid=%v num_beneficiaries=%d\n",
					cashoutTime, author, permlink, c.ID, c.NetRshares, c.TotalVoteWeight,
					c.PercentHbd, c.MaxAcceptedPayout, c.AllowCurationRewards,
					c.CashoutTime, c.LastPayout, c.WasPaid, len(c.Beneficiaries))
				return true, nil
			})
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %q\n", os.Args[2])
		os.Exit(2)
	}
}
