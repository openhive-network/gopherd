package main

// state-dump dumps the chainbase-equivalent state objects from a ghive
// data dir as JSONL — one object per line. Intended to be diff'd against
// hived's `state_snapshot` JSON output to measure parity.

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"hived-go/internal/state"
	"hived-go/internal/state/objects"
	"hived-go/internal/state/snapshot"
)

func cmdStateDump(args []string) int {
	fs := flag.NewFlagSet("state-dump", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "Pebble data dir to dump from (required)")
	out := fs.String("out", "-", "output file path (or '-' for stdout)")
	includeAccounts := fs.Bool("accounts", true, "include accounts")
	includeWitnesses := fs.Bool("witnesses", true, "include witnesses")
	includeComments := fs.Bool("comments", false, "include comments (LARGE)")
	includeDGPO := fs.Bool("dgpo", true, "include DGPO singleton")
	includeAuth := fs.Bool("auth", false, "include account_authority rows")
	loadSnap := fs.String("load-snapshot", "", "load a snapshot dir into the fresh data-dir before dumping (the typed in-memory stores are not persisted to Pebble; without --load-snapshot, accounts/dgpo/etc are empty for a Pebble-only state dir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "state-dump: --data-dir required")
		return 2
	}

	db, err := state.Open(*dataDir, 256<<20)
	if err != nil {
		fmt.Fprintf(os.Stderr, "state-dump: %v\n", err)
		return 1
	}
	defer db.Close()

	if *loadSnap != "" {
		boot := state.NewSession(db)
		objects.EnsureWitnessStore(boot)
		objects.EnsureWitnessVoteStore(boot)
		objects.EnsureAccountStore(boot)
		objects.EnsureCommentStore(boot)
		objects.EnsureCommentVoteStore(boot)
		objects.EnsureBlockSummaryStore(boot)
		objects.EnsureWitnessScheduleStore(boot)
		objects.EnsureDGPOStore(boot)
		objects.EnsureCommentCashoutScheduleStore(boot)
		objects.EnsureLimitOrderStore(boot)
		objects.EnsureVestingWithdrawalScheduleStore(boot)
		objects.EnsureTransactionStore(boot)
		objects.EnsureConvertRequestStore(boot)
		st, err := os.Stat(*loadSnap)
		if err != nil {
			fmt.Fprintf(os.Stderr, "state-dump: stat snapshot: %v\n", err)
			return 1
		}
		if st.IsDir() {
			if _, err := snapshot.Load(db, *loadSnap); err != nil {
				fmt.Fprintf(os.Stderr, "state-dump: load snapshot dir: %v\n", err)
				return 1
			}
		} else {
			if _, err := db.LoadSnapshot(*loadSnap); err != nil {
				fmt.Fprintf(os.Stderr, "state-dump: load snapshot: %v\n", err)
				return 1
			}
		}
	}

	var w *bufio.Writer
	if *out == "-" {
		w = bufio.NewWriter(os.Stdout)
	} else {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "state-dump: %v\n", err)
			return 1
		}
		defer f.Close()
		w = bufio.NewWriter(f)
	}
	defer w.Flush()
	enc := json.NewEncoder(w)

	s := state.NewSession(db)

	if *includeDGPO {
		if p, err := objects.GetDGPO(s); err == nil {
			emit(enc, "dgpo", map[string]interface{}{
				"head_block_number":       p.HeadBlockNumber,
				"head_block_id":           hex.EncodeToString(p.HeadBlockID[:]),
				"time":                    p.Time,
				"current_witness":         p.CurrentWitness,
				"current_supply":          p.CurrentSupply,
				"current_hbd_supply":      p.CurrentHBDSupply,
				"total_vesting_shares":    p.TotalVestingShares,
				"total_vesting_fund_hive": p.TotalVestingFundHive,
				"total_reward_fund_hive":  p.TotalRewardFundHive,
			})
		}
		if h, err := objects.GetFeedHistory(s); err == nil {
			emit(enc, "feed_history", map[string]interface{}{
				"price_history_len":      len(h.PriceHistory),
				"current_median_history": fmt.Sprintf("%d/%d %d/%d", h.CurrentMedianHistory.Base.Amount, h.CurrentMedianHistory.Base.Symbol.Num, h.CurrentMedianHistory.Quote.Amount, h.CurrentMedianHistory.Quote.Symbol.Num),
			})
		}
	}

	var counts = map[string]uint64{}

	if *includeAuth {
		_ = s.Range(objects.AccountAuthorityTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
			a, err := objects.ReadAccountAuthority(value)
			if err != nil {
				return true, nil
			}
			ownerKeys := make([]string, 0, len(a.Owner.KeyAuths))
			for _, k := range a.Owner.KeyAuths {
				ownerKeys = append(ownerKeys, k.Key.EncodeBase58())
			}
			activeKeys := make([]string, 0, len(a.Active.KeyAuths))
			for _, k := range a.Active.KeyAuths {
				activeKeys = append(activeKeys, k.Key.EncodeBase58())
			}
			postingKeys := make([]string, 0, len(a.Posting.KeyAuths))
			for _, k := range a.Posting.KeyAuths {
				postingKeys = append(postingKeys, k.Key.EncodeBase58())
			}
			emit(enc, "account_authority", map[string]interface{}{
				"account":               string(a.Account),
				"last_owner_update":     a.LastOwnerUpdate,
				"previous_owner_update": a.PreviousOwnerUpdate,
				"owner_threshold":       a.Owner.WeightThreshold,
				"active_threshold":      a.Active.WeightThreshold,
				"posting_threshold":     a.Posting.WeightThreshold,
				"owner_keys":            ownerKeys,
				"active_keys":           activeKeys,
				"posting_keys":          postingKeys,
				"owner_account_auths":   len(a.Owner.AccountAuths),
				"active_account_auths":  len(a.Active.AccountAuths),
				"posting_account_auths": len(a.Posting.AccountAuths),
			})
			counts["account_authorities"]++
			return true, nil
		})
	}

	if *includeAccounts {
		err := s.Range(objects.AccountTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
			a, err := objects.ReadAccount(value)
			if err != nil {
				return true, nil // skip malformed
			}
			emit(enc, "account", map[string]interface{}{
				"name":                 a.Name,
				"created":              a.Created,
				"balance":              a.Balance,
				"hbd_balance":          a.HBDBalance,
				"vesting_shares":       a.VestingShares,
				"reward_hive":          a.RewardHive,
				"reward_hbd":           a.RewardHBD,
				"reward_vests":         a.RewardVests,
				"savings_balance":      a.SavingsBalance,
				"savings_hbd_balance":  a.SavingsHBDBalance,
				"delegated_vests":      a.DelegatedVestingShares,
				"received_vests":       a.ReceivedVestingShares,
				"proxy":                a.Proxy,
				"witnesses_voted_for":  a.WitnessesVotedFor,
				"pending_claims":       a.PendingClaimedAccounts,
			})
			counts["accounts"]++
			return true, nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "state-dump accounts: %v\n", err)
			return 1
		}
	}

	if *includeWitnesses {
		err := s.Range(objects.WitnessTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
			w, err := objects.ReadWitness(value)
			if err != nil {
				return true, nil
			}
			emit(enc, "witness", map[string]interface{}{
				"owner":                  w.Owner,
				"created":                w.Created,
				"url":                    w.URL,
				"signing_key":            hex.EncodeToString(w.BlockSigningKey[:]),
				"account_creation_fee":   w.AccountCreationFee,
				"maximum_block_size":     w.MaximumBlockSize,
				"hbd_interest_rate":      w.HBDInterestRate,
				"total_votes_vests":      w.TotalVotesVests,
				"total_missed_blocks":    w.TotalMissedBlocks,
			})
			counts["witnesses"]++
			return true, nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "state-dump witnesses: %v\n", err)
			return 1
		}
	}

	if *includeComments {
		err := s.Range(objects.CommentTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
			c, err := objects.ReadComment(value)
			if err != nil {
				return true, nil
			}
			emit(enc, "comment", map[string]interface{}{
				"author":          c.Author,
				"permlink":        c.Permlink,
				"parent_author":   c.ParentAuthor,
				"parent_permlink": c.ParentPermlink,
				"created":         c.Created,
				"last_update":     c.LastUpdate,
				"net_rshares":     c.NetRshares,
				"vote_count":      c.VoteCount,
			})
			counts["comments"]++
			return true, nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "state-dump comments: %v\n", err)
			return 1
		}
	}

	w.Flush()
	fmt.Fprintf(os.Stderr, "state-dump: ")
	for k, v := range counts {
		fmt.Fprintf(os.Stderr, "%s=%d ", k, v)
	}
	fmt.Fprintln(os.Stderr)
	return 0
}

func emit(enc *json.Encoder, typeName string, body map[string]interface{}) {
	body["type"] = typeName
	_ = enc.Encode(body)
}
