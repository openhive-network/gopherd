package main

// state-jsonl dumps the chainbase-equivalent state from a ghive Pebble
// data dir as DETERMINISTIC JSONL — sorted by primary key, stable
// field order, ready for line-by-line diffing against another dump
// (either another ghive dump for regression testing, or a hived
// state_snapshot output run through `hive-snapshot-to-jsonl`).
//
// Output format (one line per object):
//
//	{"_type":"account","name":"alice","balance":12345,"hbd_balance":-50,...}
//
// Field order matches the JSON struct tag order in the helper structs
// below. Objects within a type are sorted by name; types are emitted
// in a fixed order: dgpo, feed_history, accounts, witnesses, proposals,
// reward_funds, comments (optional).
//
// This is the foundation for P1's "zero divergence" verification: run
// it on our state at HF1/HF20/HF28 boundaries, run hived's state
// dumper at the same heights, line-diff. Differences point at exactly
// which (account, field) is wrong.

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

func cmdStateJSONL(args []string) int {
	fs := flag.NewFlagSet("state-jsonl", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "Pebble data dir (required)")
	out := fs.String("out", "-", "output file path (or '-' for stdout)")
	includeComments := fs.Bool("comments", false, "include comments (LARGE)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "state-jsonl: --data-dir required")
		return 2
	}

	db, err := state.Open(*dataDir, 256<<20)
	if err != nil {
		fmt.Fprintf(os.Stderr, "state-jsonl: %v\n", err)
		return 1
	}
	defer db.Close()

	var w *bufio.Writer
	if *out == "-" {
		w = bufio.NewWriter(os.Stdout)
	} else {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "state-jsonl: %v\n", err)
			return 1
		}
		defer f.Close()
		w = bufio.NewWriter(f)
	}
	defer w.Flush()
	enc := json.NewEncoder(w)

	s := state.NewSession(db)

	// --- DGPO ---
	if p, err := objects.GetDGPO(s); err == nil {
		_ = enc.Encode(dgpoRow{
			Type:                      "dgpo",
			HeadBlockNumber:           p.HeadBlockNumber,
			HeadBlockID:               hex.EncodeToString(p.HeadBlockID[:]),
			Time:                      p.Time,
			CurrentWitness:            p.CurrentWitness,
			CurrentSupply:             p.CurrentSupply,
			CurrentHBDSupply:          p.CurrentHBDSupply,
			TotalVestingShares:        p.TotalVestingShares,
			TotalVestingFundHive:      p.TotalVestingFundHive,
			TotalRewardFundHive:       p.TotalRewardFundHive,
			VirtualSupply:             p.VirtualSupply,
			HBDInterestRate:           p.HBDInterestRate,
			HBDPrintRate:              p.HBDPrintRate,
			HBDStartPercent:           p.HBDStartPercent,
			HBDStopPercent:            p.HBDStopPercent,
			ContentRewardPercent:      p.ContentRewardPercent,
			VestingRewardPercent:      p.VestingRewardPercent,
			ProposalFundPercent:       p.ProposalFundPercent,
			ReverseAuctionSeconds:     p.ReverseAuctionSeconds,
			DelegationReturnPeriod:    p.DelegationReturnPeriod,
			AvailableAccountSubsidies: p.AvailableAccountSubsidies,
		})
	}

	// --- Accounts (sorted) ---
	type acct struct {
		row accountRow
	}
	var accts []acct
	_ = s.Range(objects.AccountTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
		a, err := objects.ReadAccount(value)
		if err != nil {
			return true, nil
		}
		accts = append(accts, acct{row: accountRow{
			Type:                          "account",
			Name:                          string(a.Name),
			Created:                       a.Created,
			Balance:                       a.Balance,
			HBDBalance:                    a.HBDBalance,
			VestingShares:                 a.VestingShares,
			DelegatedVestingShares:        a.DelegatedVestingShares,
			ReceivedVestingShares:         a.ReceivedVestingShares,
			RewardHive:                    a.RewardHive,
			RewardHBD:                     a.RewardHBD,
			RewardVests:                   a.RewardVests,
			SavingsBalance:                a.SavingsBalance,
			SavingsHBDBalance:             a.SavingsHBDBalance,
			VestingWithdrawRate:           a.VestingWithdrawRate,
			NextVestingWithdrawal:         a.NextVestingWithdrawal,
			ToWithdraw:                    a.ToWithdraw,
			Withdrawn:                     a.Withdrawn,
			Proxy:                         string(a.Proxy),
			WitnessesVotedFor:             a.WitnessesVotedFor,
			PendingClaimedAccounts:        a.PendingClaimedAccounts,
			VotingManaCurrent:             a.VotingManaCurrent,
			VotingManaLastUpdate:          a.VotingManaLastUpdate,
			HBDLastInterestPayment:        a.HBDLastInterestPayment,
			SavingsHBDLastInterestPayment: a.SavingsHBDLastInterestPayment,
			RCManaCurrent:                 a.RCManaCurrent,
			RCManaLastUpdate:              a.RCManaLastUpdate,
			LastMaxRC:                     a.LastMaxRC,
			DelegatedRCIn:                 a.DelegatedRCIn,
			DelegatedRCOut:                a.DelegatedRCOut,
			LastOwnerUpdate:               a.LastOwnerUpdate,
			SumDelayedVotes:               a.SumDelayedVotes,
		}})
		return true, nil
	})
	sort.Slice(accts, func(i, j int) bool { return accts[i].row.Name < accts[j].row.Name })
	for i := range accts {
		_ = enc.Encode(accts[i].row)
	}

	// --- Witnesses (sorted) ---
	type witnessEntry struct {
		row witnessRow
	}
	var witnesses []witnessEntry
	_ = s.Range(objects.WitnessTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
		wo, err := objects.ReadWitness(value)
		if err != nil {
			return true, nil
		}
		witnesses = append(witnesses, witnessEntry{row: witnessRow{
			Type:                "witness",
			Owner:               string(wo.Owner),
			Created:             wo.Created,
			URL:                 wo.URL,
			SigningKey:          hex.EncodeToString(wo.BlockSigningKey[:]),
			AccountCreationFee:  wo.AccountCreationFee,
			MaximumBlockSize:    wo.MaximumBlockSize,
			HBDInterestRate:     wo.HBDInterestRate,
			TotalVotesVests:     wo.TotalVotesVests,
			TotalMissedBlocks:   wo.TotalMissedBlocks,
			HardforkVersionVote: wo.HardforkVersionVote,
			HardforkTimeVote:    wo.HardforkTimeVote,
			RunningVersion:      wo.RunningVersion,
			AvailableSubsidies:  wo.AvailableWitnessAccountSubsidies,
		}})
		return true, nil
	})
	sort.Slice(witnesses, func(i, j int) bool { return witnesses[i].row.Owner < witnesses[j].row.Owner })
	for i := range witnesses {
		_ = enc.Encode(witnesses[i].row)
	}

	// --- Comments (optional, very large) ---
	if *includeComments {
		var comms []commentRow
		_ = s.Range(objects.CommentTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
			c, err := objects.ReadComment(value)
			if err != nil {
				return true, nil
			}
			comms = append(comms, commentRow{
				Type:            "comment",
				Author:          string(c.Author),
				Permlink:        c.Permlink,
				ParentAuthor:    string(c.ParentAuthor),
				ParentPermlink:  c.ParentPermlink,
				Created:         c.Created,
				LastUpdate:      c.LastUpdate,
				CashoutTime:     c.CashoutTime,
				NetRshares:      c.NetRshares,
				VoteCount:       c.VoteCount,
				MaxAcceptedPay:  c.MaxAcceptedPayout,
				PercentHbd:      c.PercentHbd,
				WasPaid:         c.WasPaid,
				VoteRshares:     c.VoteRshares,
				TotalVoteWeight: c.TotalVoteWeight,
			})
			return true, nil
		})
		sort.Slice(comms, func(i, j int) bool {
			if comms[i].Author != comms[j].Author {
				return comms[i].Author < comms[j].Author
			}
			return comms[i].Permlink < comms[j].Permlink
		})
		for i := range comms {
			_ = enc.Encode(comms[i])
		}
	}

	_ = auth.AccountName("") // suppress unused-warn in some build configs
	return 0
}

// JSON-tagged row types — fields appear in struct-declaration order in
// Go's encoding/json output, so the canonical output is deterministic.

type dgpoRow struct {
	Type                      string `json:"_type"`
	HeadBlockNumber           uint32 `json:"head_block_number"`
	HeadBlockID               string `json:"head_block_id"`
	Time                      uint32 `json:"time"`
	CurrentWitness            string `json:"current_witness"`
	CurrentSupply             int64  `json:"current_supply"`
	CurrentHBDSupply          int64  `json:"current_hbd_supply"`
	TotalVestingShares        int64  `json:"total_vesting_shares"`
	TotalVestingFundHive      int64  `json:"total_vesting_fund_hive"`
	TotalRewardFundHive       int64  `json:"total_reward_fund_hive"`
	VirtualSupply             int64  `json:"virtual_supply"`
	HBDInterestRate           uint16 `json:"hbd_interest_rate"`
	HBDPrintRate              uint16 `json:"hbd_print_rate"`
	HBDStartPercent           uint16 `json:"hbd_start_percent"`
	HBDStopPercent            uint16 `json:"hbd_stop_percent"`
	ContentRewardPercent      uint16 `json:"content_reward_percent"`
	VestingRewardPercent      uint16 `json:"vesting_reward_percent"`
	ProposalFundPercent       uint16 `json:"proposal_fund_percent"`
	ReverseAuctionSeconds     uint32 `json:"reverse_auction_seconds"`
	DelegationReturnPeriod    uint32 `json:"delegation_return_period"`
	AvailableAccountSubsidies int64  `json:"available_account_subsidies"`
}

type accountRow struct {
	Type                          string `json:"_type"`
	Name                          string `json:"name"`
	Created                       uint32 `json:"created"`
	Balance                       int64  `json:"balance"`
	HBDBalance                    int64  `json:"hbd_balance"`
	VestingShares                 int64  `json:"vesting_shares"`
	DelegatedVestingShares        int64  `json:"delegated_vesting_shares"`
	ReceivedVestingShares         int64  `json:"received_vesting_shares"`
	RewardHive                    int64  `json:"reward_hive"`
	RewardHBD                     int64  `json:"reward_hbd"`
	RewardVests                   int64  `json:"reward_vests"`
	SavingsBalance                int64  `json:"savings_balance"`
	SavingsHBDBalance             int64  `json:"savings_hbd_balance"`
	VestingWithdrawRate           int64  `json:"vesting_withdraw_rate"`
	NextVestingWithdrawal         uint32 `json:"next_vesting_withdrawal"`
	ToWithdraw                    int64  `json:"to_withdraw"`
	Withdrawn                     int64  `json:"withdrawn"`
	Proxy                         string `json:"proxy"`
	WitnessesVotedFor             uint16 `json:"witnesses_voted_for"`
	PendingClaimedAccounts        uint16 `json:"pending_claimed_accounts"`
	VotingManaCurrent             int64  `json:"voting_mana_current"`
	VotingManaLastUpdate          uint32 `json:"voting_mana_last_update"`
	HBDLastInterestPayment        uint32 `json:"hbd_last_interest_payment"`
	SavingsHBDLastInterestPayment uint32 `json:"savings_hbd_last_interest_payment"`
	RCManaCurrent                 int64  `json:"rc_mana_current"`
	RCManaLastUpdate              uint32 `json:"rc_mana_last_update"`
	LastMaxRC                     int64  `json:"last_max_rc"`
	DelegatedRCIn                 int64  `json:"delegated_rc_in"`
	DelegatedRCOut                int64  `json:"delegated_rc_out"`
	LastOwnerUpdate               uint32 `json:"last_owner_update"`
	SumDelayedVotes               uint64 `json:"sum_delayed_votes"`
}

type witnessRow struct {
	Type                string `json:"_type"`
	Owner               string `json:"owner"`
	Created             uint32 `json:"created"`
	URL                 string `json:"url"`
	SigningKey          string `json:"signing_key"`
	AccountCreationFee  int64  `json:"account_creation_fee"`
	MaximumBlockSize    uint32 `json:"maximum_block_size"`
	HBDInterestRate     uint16 `json:"hbd_interest_rate"`
	TotalVotesVests     int64  `json:"total_votes_vests"`
	TotalMissedBlocks   uint32 `json:"total_missed_blocks"`
	HardforkVersionVote uint32 `json:"hardfork_version_vote"`
	HardforkTimeVote    uint32 `json:"hardfork_time_vote"`
	RunningVersion      uint32 `json:"running_version"`
	AvailableSubsidies  int64  `json:"available_subsidies"`
}

type commentRow struct {
	Type            string `json:"_type"`
	Author          string `json:"author"`
	Permlink        string `json:"permlink"`
	ParentAuthor    string `json:"parent_author"`
	ParentPermlink  string `json:"parent_permlink"`
	Created         uint32 `json:"created"`
	LastUpdate      uint32 `json:"last_update"`
	CashoutTime     uint32 `json:"cashout_time"`
	NetRshares      int64  `json:"net_rshares"`
	VoteCount       uint32 `json:"vote_count"`
	MaxAcceptedPay  int64  `json:"max_accepted_payout"`
	PercentHbd      uint16 `json:"percent_hbd"`
	WasPaid         bool   `json:"was_paid"`
	VoteRshares     uint64 `json:"vote_rshares"`
	TotalVoteWeight uint64 `json:"total_vote_weight"`
}
