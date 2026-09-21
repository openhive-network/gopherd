package main

// state-jsonl dumps the chainbase-equivalent state from a ghive Pebble
// data dir as DETERMINISTIC JSONL — sorted by identifier, stable field
// order, ready for line-by-line diffing against another dump (either
// another ghive dump for regression testing, or a hived-derived dump
// produced by `ghive-tools hived-jsonl`).
//
// Output format (one line per object):
//
//	{"_type":"account","name":"alice","balance":12345,"hbd_balance":-50,...}
//
// Every row's sort key is computed by the SAME identKey() the differ
// uses (both live in package main), so the file is always in the exact
// order the lockstep snapshot-diff reader expects — no matter which
// object types are present. That is what makes a FULL-state diff (every
// account, order, escrow, proposal, …) align correctly, surfacing the
// diverging (object, field) for objects nobody thought to name.
//
// Covered types: dgpo, feed_history, accounts, witnesses, reward_funds,
// limit_orders, escrows, convert_requests, collateralized_converts,
// savings_withdraws, vesting_delegation_expirations, proposals, and
// (optional) comments.

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
	"gopherd/internal/state/snapshot"
	"gopherd/internal/statejsonl"
)

// registerAllStores pre-registers every typed subsystem on a freshly
// opened Database so a directory snapshot loads its full store set
// (accounts, witnesses, comments, limit orders, …, which live in memory
// rather than pebble K/V). Mirrors replay's snapshot-load bootstrap.
func registerAllStores(db *state.Database) {
	boot := state.NewSession(db)
	objects.EnsureWitnessStore(boot)
	objects.EnsureWitnessVoteStore(boot)
	objects.EnsureAccountStore(boot)
	objects.EnsureCommentStore(boot)
	objects.EnsureCommentVoteStore(boot)
	objects.EnsureBlockSummaryStore(boot)
	objects.EnsureWitnessScheduleStore(boot)
	objects.EnsureFutureWitnessScheduleStore(boot)
	objects.EnsureDGPOStore(boot)
	objects.EnsureCommentCashoutScheduleStore(boot)
	objects.EnsureLimitOrderStore(boot)
	objects.EnsureVestingWithdrawalScheduleStore(boot)
	objects.EnsureTransactionStore(boot)
	objects.EnsureConvertRequestStore(boot)
}

// jrow is a marshaled object paired with its canonical identKey for
// sorting. Shared by state-jsonl and hived-jsonl.
type jrow struct {
	key  string
	data []byte
}

func cmdStateJSONL(args []string) int {
	fs := flag.NewFlagSet("state-jsonl", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "Pebble data dir (raw K/V only — misses typed stores; prefer --snapshot)")
	snapDir := fs.String("snapshot", "", "directory snapshot produced by `replay --snapshot-every` (loads full typed-store state; the correct input for a full-state diff)")
	out := fs.String("out", "-", "output file path (or '-' for stdout)")
	includeComments := fs.Bool("comments", false, "include comments (LARGE)")
	accountsOnly := fs.Bool("accounts-only", false, "load ONLY account_store.bin from --snapshot and emit account rows (skips the multi-GB comment store; for manabar/balance diffs)")
	votesOf := fs.String("votes-of", "", "if set as author/permlink, dump just that comment's votes (voter,rshares,weight%,curation_weight) and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dataDir == "" && *snapDir == "" {
		fmt.Fprintln(os.Stderr, "state-jsonl: --snapshot (preferred) or --data-dir required")
		return 2
	}

	// --accounts-only: load just account_store.bin (the full snapshot's
	// comment_store can be tens of GB and OOM the box; the manabar/balance
	// diff only needs accounts). Bypasses snapshot.Load entirely.
	if *accountsOnly {
		if *snapDir == "" {
			fmt.Fprintln(os.Stderr, "state-jsonl: --accounts-only requires --snapshot")
			return 2
		}
		return dumpAccountsOnly(*snapDir, *out)
	}

	var db *state.Database
	if *snapDir != "" {
		db = state.OpenMem()
		registerAllStores(db)
		if _, err := snapshot.Load(db, *snapDir); err != nil {
			fmt.Fprintf(os.Stderr, "state-jsonl: load snapshot %s: %v\n", *snapDir, err)
			return 1
		}
	} else {
		var err error
		db, err = state.Open(*dataDir, 256<<20)
		if err != nil {
			fmt.Fprintf(os.Stderr, "state-jsonl: %v\n", err)
			return 1
		}
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

	s := state.NewSession(db)

	// --votes-of: dump one comment's votes and exit (curation-weight drill).
	if *votesOf != "" {
		slash := strings.IndexByte(*votesOf, '/')
		if slash < 0 {
			fmt.Fprintln(os.Stderr, "state-jsonl: --votes-of must be author/permlink")
			return 2
		}
		va := auth.AccountName((*votesOf)[:slash])
		vp := (*votesOf)[slash+1:]
		if c, ok := objects.GetCommentRef(s, va, vp); ok {
			fmt.Printf("comment %s/%s net_rshares=%d vote_rshares=%d total_vote_weight=%d vote_count=%d cashout=%d\n",
				va, vp, c.NetRshares, c.VoteRshares, c.TotalVoteWeight, c.VoteCount, c.CashoutTime)
		}
		_ = objects.RangeCommentVotesByComment(s, va, vp, func(cv *objects.CommentVote) (bool, error) {
			fmt.Printf("VOTE voter=%s rshares=%d weight_pct=%d curation_weight=%d last_update=%d num_changes=%d\n",
				cv.Voter, cv.Rshares, cv.Weight, cv.CurationWeight, cv.LastUpdate, cv.NumChanges)
			return true, nil
		})
		return 0
	}

	// Unified collect → sort → emit. Sort key = identKey(marshaled row),
	// the same function the differ aligns on.
	var rows []jrow
	add := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		rows = append(rows, jrow{key: statejsonl.IdentKey(b), data: append([]byte(nil), b...)})
	}

	// --- DGPO ---
	if p, err := objects.GetDGPO(s); err == nil {
		add(statejsonl.DGPORow{
			Type:                         "dgpo",
			HeadBlockNumber:              p.HeadBlockNumber,
			HeadBlockID:                  hex.EncodeToString(p.HeadBlockID[:]),
			Time:                         p.Time,
			CurrentWitness:               p.CurrentWitness,
			CurrentSupply:                p.CurrentSupply,
			CurrentHBDSupply:             p.CurrentHBDSupply,
			TotalVestingShares:           p.TotalVestingShares,
			TotalVestingFundHive:         p.TotalVestingFundHive,
			TotalRewardFundHive:          p.TotalRewardFundHive,
			VirtualSupply:                p.VirtualSupply,
			PendingRewardedVestingShares: p.PendingRewardedVestingShares,
			PendingRewardedVestingHive:   p.PendingRewardedVestingHive,
			DHFIntervalLedger:            p.DHFIntervalLedger,
			HBDInterestRate:              p.HBDInterestRate,
			HBDPrintRate:                 p.HBDPrintRate,
			HBDStartPercent:              p.HBDStartPercent,
			HBDStopPercent:               p.HBDStopPercent,
			ContentRewardPercent:         p.ContentRewardPercent,
			VestingRewardPercent:         p.VestingRewardPercent,
			ProposalFundPercent:          p.ProposalFundPercent,
			ReverseAuctionSeconds:        p.ReverseAuctionSeconds,
			DelegationReturnPeriod:       p.DelegationReturnPeriod,
			AvailableAccountSubsidies:    p.AvailableAccountSubsidies,
		})
	}

	// --- feed_history (singleton) ---
	if fh, err := objects.GetFeedHistory(s); err == nil {
		var last statejsonl.PriceJSON
		if n := len(fh.PriceHistory); n > 0 {
			last = priceRow(fh.PriceHistory[n-1])
		}
		add(statejsonl.FeedHistoryRow{
			Type:             "feed_history",
			CurrentMedian:    priceRow(fh.CurrentMedianHistory),
			MarketMedian:     priceRow(fh.MarketMedianHistory),
			CurrentMin:       priceRow(fh.CurrentMinHistory),
			CurrentMax:       priceRow(fh.CurrentMaxHistory),
			PriceHistoryLen:  len(fh.PriceHistory),
			PriceHistoryLast: last,
		})
	}

	// --- Accounts (typed store) ---
	_ = objects.RangeAccounts(s, func(a *objects.Account) (bool, error) {
		add(statejsonl.AccountRow{
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
		})
		return true, nil
	})

	// --- Witnesses (typed store) ---
	_ = objects.EnsureWitnessStore(s).RangePrimary(func(wo *objects.Witness) (bool, error) {
		add(statejsonl.WitnessRow{
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
		})
		return true, nil
	})

	// --- Reward funds ---
	_ = s.Range(objects.RewardFundTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
		rf, err := objects.ReadRewardFund(value)
		if err != nil {
			return true, nil
		}
		add(statejsonl.RewardFundRow{
			Type:                  "reward_fund",
			Name:                  rf.Name,
			RewardBalance:         rf.RewardBalance,
			RecentClaims:          hex.EncodeToString(rf.RecentClaims[:]),
			LastUpdate:            rf.LastUpdate,
			ContentConstant:       rf.ContentConstant,
			PercentCuration:       rf.PercentCurationRewards,
			PercentContent:        rf.PercentContentRewards,
			AuthorRewardCurve:     uint8(rf.AuthorRewardCurve),
			CurationRewardCurve:   uint8(rf.CurationRewardCurve),
			DownvoteRewardBalance: rf.DownvoteRewardBalance,
		})
		return true, nil
	})

	// --- Limit orders ---
	_ = objects.EnsureLimitOrderStore(s).RangeByExpiration(func(lo *objects.LimitOrder) (bool, error) {
		add(limitOrderRow{
			Type:          "limit_order",
			Owner:         string(lo.Owner),
			OrderID:       lo.OrderID,
			Created:       lo.Created,
			Expiration:    lo.Expiration,
			ForSale:       lo.ForSaleAmount,
			ForSaleSym:    lo.ForSaleSymbol,
			MinReceive:    lo.MinReceiveAmt,
			MinReceiveSym: lo.MinReceiveSym,
			SellPriceBase: lo.SellPriceBaseAmount,
		})
		return true, nil
	})

	// --- Escrows ---
	_ = s.Range(objects.EscrowTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
		e, err := objects.ReadEscrow(value)
		if err != nil {
			return true, nil
		}
		add(escrowRow{
			Type:                 "escrow",
			From:                 string(e.From),
			To:                   string(e.To),
			Agent:                string(e.Agent),
			EscrowID:             e.EscrowID,
			HBDAmount:            e.HBDAmount,
			HiveAmount:           e.HiveAmount,
			PendingFee:           e.PendingFee,
			PendingFeeIsHBD:      e.PendingFeeIsHBD,
			RatificationDeadline: e.RatificationDeadline,
			Expiration:           e.EscrowExpiration,
			ToApproved:           e.ToApproved,
			AgentApproved:        e.AgentApproved,
			Disputed:             e.Disputed,
		})
		return true, nil
	})

	// --- Convert requests ---
	objects.EnsureConvertRequestStore(s).RangeDueBefore(^uint32(0), func(c objects.ConvertRequest) bool {
		add(convertRow{
			Type:       "convert_request",
			Owner:      string(c.Owner),
			RequestID:  c.RequestID,
			Amount:     c.Amount,
			Conversion: c.Conversion,
		})
		return true
	})

	// --- Collateralized converts ---
	_ = objects.RangeAllCollateralizedConvert(s, func(c objects.CollateralizedConvert) bool {
		add(collConvertRow{
			Type:       "collateralized_convert",
			Owner:      string(c.Owner),
			RequestID:  c.RequestID,
			Collateral: c.Collateral,
			HBDOut:     c.HBDOut,
			Conversion: c.Conversion,
		})
		return true
	})

	// --- Savings withdrawals ---
	_ = s.Range(objects.SavingsWithdrawTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
		sw, err := objects.ReadSavingsWithdraw(value)
		if err != nil {
			return true, nil
		}
		add(savingsWithdrawRow{
			Type:      "savings_withdraw",
			From:      string(sw.From),
			RequestID: sw.RequestID,
			To:        string(sw.To),
			Amount:    sw.Amount,
			Symbol:    sw.Symbol,
			Complete:  sw.Complete,
		})
		return true, nil
	})

	// --- Vesting-delegation return objects ---
	_ = s.Range(objects.VestingDelegationExpirationTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
		e, err := objects.ReadVestingDelegationExpiration(value)
		if err != nil {
			return true, nil
		}
		add(vdeRow{
			Type:          "vesting_delegation_expiration",
			ID:            e.ID,
			Delegator:     string(e.Delegator),
			VestingShares: e.VestingShares,
			Expiration:    e.Expiration,
		})
		return true, nil
	})

	// --- Proposals (DHF) ---
	_ = s.Range(objects.ProposalTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
		p, err := objects.ReadProposal(value)
		if err != nil {
			return true, nil
		}
		add(proposalRow{
			Type:       "proposal",
			ID:         p.ID,
			Creator:    string(p.Creator),
			Receiver:   string(p.Receiver),
			StartDate:  p.StartDate,
			EndDate:    p.EndDate,
			DailyPay:   p.DailyPay,
			Subject:    p.Subject,
			Permlink:   p.Permlink,
			TotalVotes: p.TotalVotes,
			Removed:    p.Removed,
		})
		return true, nil
	})

	// --- Comments (optional, very large; typed store) ---
	if *includeComments {
		_ = objects.EnsureCommentStore(s).RangePrimary(func(c *objects.Comment) (bool, error) {
			add(commentRow{
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
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })
	for i := range rows {
		w.Write(rows[i].data)
		w.WriteByte('\n')
	}
	return 0
}

// priceRow flattens an asset.Price into comparable scalar fields.
func priceRow(p asset.Price) statejsonl.PriceJSON {
	return statejsonl.PriceJSON{
		BaseAmount:  p.Base.Amount,
		BaseSym:     uint32(p.Base.Symbol.Num),
		QuoteAmount: p.Quote.Amount,
		QuoteSym:    uint32(p.Quote.Symbol.Num),
	}
}

// JSON-tagged row types — fields appear in struct-declaration order in
// Go's encoding/json output, so the canonical output is deterministic.
// The shared types (dgpo, feed_history, account, witness, reward_fund,
// price) live in internal/statejsonl so the hived-side dumper emits a
// byte-identical format. The types below are ghive-only for now.

type limitOrderRow struct {
	Type          string `json:"_type"`
	Owner         string `json:"owner"`
	OrderID       uint32 `json:"order_id"`
	Created       uint32 `json:"created"`
	Expiration    uint32 `json:"expiration"`
	ForSale       int64  `json:"for_sale"`
	ForSaleSym    uint32 `json:"for_sale_sym"`
	MinReceive    int64  `json:"min_receive"`
	MinReceiveSym uint32 `json:"min_receive_sym"`
	SellPriceBase int64  `json:"sell_price_base"`
}

type escrowRow struct {
	Type                 string `json:"_type"`
	From                 string `json:"from"`
	To                   string `json:"to"`
	Agent                string `json:"agent"`
	EscrowID             uint32 `json:"escrow_id"`
	HBDAmount            int64  `json:"hbd_amount"`
	HiveAmount           int64  `json:"hive_amount"`
	PendingFee           int64  `json:"pending_fee"`
	PendingFeeIsHBD      bool   `json:"pending_fee_is_hbd"`
	RatificationDeadline uint32 `json:"ratification_deadline"`
	Expiration           uint32 `json:"expiration"`
	ToApproved           bool   `json:"to_approved"`
	AgentApproved        bool   `json:"agent_approved"`
	Disputed             bool   `json:"disputed"`
}

type convertRow struct {
	Type       string `json:"_type"`
	Owner      string `json:"owner"`
	RequestID  uint32 `json:"request_id"`
	Amount     int64  `json:"amount"`
	Conversion uint32 `json:"conversion"`
}

type collConvertRow struct {
	Type       string `json:"_type"`
	Owner      string `json:"owner"`
	RequestID  uint32 `json:"request_id"`
	Collateral int64  `json:"collateral"`
	HBDOut     int64  `json:"hbd_out"`
	Conversion uint32 `json:"conversion"`
}

type savingsWithdrawRow struct {
	Type      string `json:"_type"`
	From      string `json:"from"`
	RequestID uint32 `json:"request_id"`
	To        string `json:"to"`
	Amount    int64  `json:"amount"`
	Symbol    uint32 `json:"symbol"`
	Complete  uint32 `json:"complete"`
}

type vdeRow struct {
	Type          string `json:"_type"`
	ID            uint32 `json:"id"`
	Delegator     string `json:"delegator"`
	VestingShares int64  `json:"vesting_shares"`
	Expiration    uint32 `json:"expiration"`
}

type proposalRow struct {
	Type       string `json:"_type"`
	ID         int64  `json:"id"`
	Creator    string `json:"creator"`
	Receiver   string `json:"receiver"`
	StartDate  uint32 `json:"start_date"`
	EndDate    uint32 `json:"end_date"`
	DailyPay   int64  `json:"daily_pay"`
	Subject    string `json:"subject"`
	Permlink   string `json:"permlink"`
	TotalVotes int64  `json:"total_votes"`
	// omitempty so the common case (nothing flagged) keeps the dump
	// byte-identical to what it produced before the field existed, while a
	// proposal left flagged by a budget-limited removal becomes visible to
	// the state diff instead of silently matching.
	Removed bool `json:"removed,omitempty"`
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

// dumpAccountsOnly loads ONLY the account_store.bin file from a directory
// snapshot and emits the canonical account JSONL rows, sorted by ident
// key — byte-identical to the account subset of a full state-jsonl dump,
// but without loading the (multi-GB) comment store that OOMs the box.
func dumpAccountsOnly(snapDir, outPath string) int {
	f, err := os.Open(snapDir + "/subsystems/account_store.bin")
	if err != nil {
		fmt.Fprintf(os.Stderr, "state-jsonl: open account_store.bin: %v\n", err)
		return 1
	}
	defer f.Close()
	db := state.OpenMem()
	defer db.Close()
	s := state.NewSession(db)
	store := objects.EnsureAccountStore(s)
	if err := store.UnmarshalSnapshot(bufio.NewReaderSize(f, 1<<20)); err != nil {
		fmt.Fprintf(os.Stderr, "state-jsonl: unmarshal account_store: %v\n", err)
		return 1
	}

	var rows []jrow
	_ = objects.RangeAccounts(s, func(a *objects.Account) (bool, error) {
		v := statejsonl.AccountRow{
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
		}
		b, merr := json.Marshal(v)
		if merr != nil {
			return true, nil
		}
		rows = append(rows, jrow{key: statejsonl.IdentKey(b), data: append([]byte(nil), b...)})
		return true, nil
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })

	var w *bufio.Writer
	if outPath == "-" {
		w = bufio.NewWriter(os.Stdout)
	} else {
		of, oerr := os.Create(outPath)
		if oerr != nil {
			fmt.Fprintf(os.Stderr, "state-jsonl: create out: %v\n", oerr)
			return 1
		}
		defer of.Close()
		w = bufio.NewWriter(of)
	}
	defer w.Flush()
	for _, r := range rows {
		w.Write(r.data)
		w.WriteByte('\n')
	}
	fmt.Fprintf(os.Stderr, "state-jsonl: wrote %d account rows\n", len(rows))
	return 0
}
