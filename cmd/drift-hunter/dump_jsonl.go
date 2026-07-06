package main

// dump-jsonl mode: pull FULL chain state from a hived checkpoint over
// JSON-RPC and write it as the canonical statejsonl format, so it can be
// line-diffed against `ghive-tools state-jsonl` output via
// `ghive-tools snapshot-diff`. This is the hived side of the P1
// full-state diff — the piece that surfaces a diverging (object, field)
// for accounts nobody named, which is what catches the dominant
// conservation-preserving redistribution drift.
//
// Coverage: dgpo, accounts (paged), witnesses (paged), feed_history,
// reward_funds — the objects hived exposes with full fidelity here.
// Limit orders / escrows / converts / savings / proposals are TODO (they
// need list_* pagination and are lower drift-value). Account fields that
// live outside account_api (RC manabar, HBD interest stamps) are left
// zero and dropped by snapshot-diff's default --skip-fields.
//
// NOTE: this RPC mapping is validated against a live hived, which is only
// available where the long block_log lives. It compiles and reuses the
// proven hivedAsset/parseISO8601Sec/curve helpers, but field names should
// be sanity-checked on first real run.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"hived-go/internal/state/objects"
	"hived-go/internal/statejsonl"
)

// dumpRow pairs a marshaled JSONL line with its canonical sort key.
type dumpRow struct {
	key  string
	data []byte
}

func dumpHivedJSONL(rpc *rpcClient, outPath string) error {
	var rows []dumpRow
	add := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		rows = append(rows, dumpRow{key: statejsonl.IdentKey(b), data: b})
		return nil
	}

	// --- DGPO ---
	dgpo, err := rpc.fetchDGPO()
	if err != nil {
		return fmt.Errorf("dgpo: %w", err)
	}
	cs, _ := dgpo.CurrentSupply.AmountInt()
	chs, _ := dgpo.CurrentHBDSupply.AmountInt()
	tvs, _ := dgpo.TotalVestingShares.AmountInt()
	tvf, _ := dgpo.TotalVestingFundHive.AmountInt()
	trf, _ := dgpo.TotalRewardFundHive.AmountInt()
	vs, _ := dgpo.VirtualSupply.AmountInt()
	prvs, _ := dgpo.PendingRewardedVestingShares.AmountInt()
	prvh, _ := dgpo.PendingRewardedVestingHive.AmountInt()
	if err := add(statejsonl.DGPORow{
		Type:                         "dgpo",
		HeadBlockNumber:              dgpo.HeadBlockNumber,
		HeadBlockID:                  dgpo.HeadBlockID,
		Time:                         parseISO8601Sec(dgpo.Time),
		CurrentWitness:               dgpo.CurrentWitness,
		CurrentSupply:                cs,
		CurrentHBDSupply:             chs,
		TotalVestingShares:           tvs,
		TotalVestingFundHive:         tvf,
		TotalRewardFundHive:          trf,
		VirtualSupply:                vs,
		PendingRewardedVestingShares: prvs,
		PendingRewardedVestingHive:   prvh,
		DHFIntervalLedger:            looseInt(string(dgpo.DHFIntervalLedger)),
		HBDInterestRate:              dgpo.HBDInterestRate,
		HBDPrintRate:                 dgpo.HBDPrintRate,
		HBDStartPercent:              dgpo.HBDStartPercent,
		HBDStopPercent:               dgpo.HBDStopPercent,
		ContentRewardPercent:         dgpo.ContentRewardPercent,
		VestingRewardPercent:         dgpo.VestingRewardPercent,
		ProposalFundPercent:          dgpo.ProposalFundPercent,
		ReverseAuctionSeconds:        dgpo.ReverseAuctionSeconds,
		DelegationReturnPeriod:       dgpo.DelegationReturnPeriod,
		AvailableAccountSubsidies:    dgpo.AvailableAccountSubsidies,
	}); err != nil {
		return err
	}

	// --- feed_history ---
	if fh, err := rpc.fetchFeedHistory(); err == nil {
		var last statejsonl.PriceJSON
		if n := len(fh.PriceHistory); n > 0 {
			last = hivedPriceRow(fh.PriceHistory[n-1])
		}
		_ = add(statejsonl.FeedHistoryRow{
			Type:             "feed_history",
			CurrentMedian:    hivedPriceRow(fh.CurrentMedianHistory),
			MarketMedian:     hivedPriceRow(fh.MarketMedianHistory),
			CurrentMin:       hivedPriceRow(fh.CurrentMinHistory),
			CurrentMax:       hivedPriceRow(fh.CurrentMaxHistory),
			PriceHistoryLen:  len(fh.PriceHistory),
			PriceHistoryLast: last,
		})
	}

	// --- Accounts (paged) ---
	accts, err := rpc.fetchAllAccounts()
	if err != nil {
		return fmt.Errorf("accounts: %w", err)
	}
	for i := range accts {
		a := &accts[i]
		bal, _ := a.Balance.AmountInt()
		hbd, _ := a.HBDBalance.AmountInt()
		sav, _ := a.SavingsBalance.AmountInt()
		savHbd, _ := a.SavingsHBDBalance.AmountInt()
		vest, _ := a.VestingShares.AmountInt()
		delv, _ := a.DelegatedVestingShares.AmountInt()
		recv, _ := a.ReceivedVestingShares.AmountInt()
		rh, _ := a.RewardHiveBalance.AmountInt()
		rhbd, _ := a.RewardHBDBalance.AmountInt()
		rv, _ := a.RewardVestingBalance.AmountInt()
		wr, _ := a.VestingWithdrawRate.AmountInt()
		_ = add(statejsonl.AccountRow{
			Type:                   "account",
			Name:                   a.Name,
			Created:                parseISO8601Sec(a.Created),
			Balance:                bal,
			HBDBalance:             hbd,
			VestingShares:          vest,
			DelegatedVestingShares: delv,
			ReceivedVestingShares:  recv,
			RewardHive:             rh,
			RewardHBD:              rhbd,
			RewardVests:            rv,
			SavingsBalance:         sav,
			SavingsHBDBalance:      savHbd,
			VestingWithdrawRate:    wr,
			NextVestingWithdrawal:  parseISO8601Sec(a.NextVestingWithdrawal),
			ToWithdraw:             looseInt(a.ToWithdraw.String()),
			Withdrawn:              looseInt(a.Withdrawn.String()),
			Proxy:                  a.Proxy,
			WitnessesVotedFor:      a.WitnessesVotedFor,
			PendingClaimedAccounts: int64(a.PendingClaimedAccounts),
			VotingManaCurrent:      looseInt(a.VotingManabar.CurrentMana.String()),
			VotingManaLastUpdate:   a.VotingManabar.LastUpdateTime,
			LastOwnerUpdate:        parseISO8601Sec(a.LastOwnerUpdate),
			SumDelayedVotes:        uint64(looseInt(a.SumDelayedVotes.String())),
			// RC manabar + HBD interest stamps are not in account_api; left
			// zero and dropped by snapshot-diff --skip-fields.
		})
	}

	// --- Witnesses (paged) ---
	wits, err := rpc.fetchWitnessesByName(0)
	if err != nil {
		return fmt.Errorf("witnesses: %w", err)
	}
	for i := range wits {
		wv := &wits[i]
		fee, _ := wv.Props.AccountCreationFee.AmountInt()
		_ = add(statejsonl.WitnessRow{
			Type:                "witness",
			Owner:               wv.Owner,
			Created:             parseISO8601Sec(wv.Created),
			URL:                 wv.URL,
			SigningKey:          pubkeyB58ToHex(wv.SigningKey),
			AccountCreationFee:  fee,
			MaximumBlockSize:    wv.Props.MaximumBlockSize,
			HBDInterestRate:     wv.Props.HBDInterestRate,
			TotalVotesVests:     looseInt(wv.Votes.String()),
			TotalMissedBlocks:   wv.TotalMissedBlocks,
			HardforkVersionVote: 0, // packed-version string; not cross-checked here
			HardforkTimeVote:    parseISO8601Sec(wv.HardforkTimeVote),
			RunningVersion:      0, // packed-version string; not cross-checked here
			AvailableSubsidies:  wv.AvailableWitnessAccountSubsidies,
		})
	}

	// --- Reward funds ---
	if funds, err := rpc.fetchRewardFunds(); err == nil {
		for i := range funds {
			rf := &funds[i]
			bal, _ := rf.RewardBalance.AmountInt()
			claims, _ := hivedDecimalToUint128BE(rf.RecentClaims.String())
			_ = add(statejsonl.RewardFundRow{
				Type:                "reward_fund",
				Name:                rf.Name,
				RewardBalance:       bal,
				RecentClaims:        hexLower(claims[:]),
				LastUpdate:          parseISO8601Sec(rf.LastUpdate),
				ContentConstant:     looseInt(rf.ContentConstant.String()),
				PercentCuration:     rf.PercentCurationRewards,
				PercentContent:      rf.PercentContentRewards,
				AuthorRewardCurve:   curveStrToID(rf.AuthorRewardCurve),
				CurationRewardCurve: curveStrToID(rf.CurationRewardCurve),
				// downvote_reward_balance lives in a companion object; TODO.
			})
		}
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	for i := range rows {
		w.Write(rows[i].data)
		w.WriteByte('\n')
	}
	fmt.Fprintf(os.Stderr, "dump-jsonl: wrote %d rows (%d accounts, %d witnesses) to %s\n",
		len(rows), len(accts), len(wits), outPath)
	return nil
}

// dumpHivedAccount is the full-fidelity account_api row for dumping.
type dumpHivedAccount struct {
	Name                   string      `json:"name"`
	Created                string      `json:"created"`
	Balance                hivedAsset  `json:"balance"`
	HBDBalance             hivedAsset  `json:"hbd_balance"`
	SavingsBalance         hivedAsset  `json:"savings_balance"`
	SavingsHBDBalance      hivedAsset  `json:"savings_hbd_balance"`
	VestingShares          hivedAsset  `json:"vesting_shares"`
	DelegatedVestingShares hivedAsset  `json:"delegated_vesting_shares"`
	ReceivedVestingShares  hivedAsset  `json:"received_vesting_shares"`
	RewardHiveBalance      hivedAsset  `json:"reward_hive_balance"`
	RewardHBDBalance       hivedAsset  `json:"reward_hbd_balance"`
	RewardVestingBalance   hivedAsset  `json:"reward_vesting_balance"`
	VestingWithdrawRate    hivedAsset  `json:"vesting_withdraw_rate"`
	NextVestingWithdrawal  string      `json:"next_vesting_withdrawal"`
	ToWithdraw             json.Number `json:"to_withdraw"`
	Withdrawn              json.Number `json:"withdrawn"`
	Proxy                  string      `json:"proxy"`
	WitnessesVotedFor      uint16      `json:"witnesses_voted_for"`
	PendingClaimedAccounts uint16      `json:"pending_claimed_accounts"`
	LastOwnerUpdate        string      `json:"last_owner_update"`
	SumDelayedVotes        json.Number `json:"sum_delayed_votes"`
	VotingManabar          struct {
		CurrentMana    json.Number `json:"current_mana"`
		LastUpdateTime uint32      `json:"last_update_time"`
	} `json:"voting_manabar"`
}

// fetchAllAccounts pages database_api.list_accounts sorted by_name.
func (c *rpcClient) fetchAllAccounts() ([]dumpHivedAccount, error) {
	var all []dumpHivedAccount
	start := ""
	const pageSize = 1000
	for {
		params := map[string]interface{}{"start": start, "limit": pageSize, "order": "by_name"}
		raw, err := c.call("database_api.list_accounts", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Accounts []dumpHivedAccount `json:"accounts"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("decode list_accounts: %w", err)
		}
		if len(page.Accounts) == 0 {
			break
		}
		if len(all) == 0 {
			all = append(all, page.Accounts...)
		} else {
			all = append(all, page.Accounts[1:]...) // first row = resume cursor
		}
		if len(page.Accounts) < pageSize {
			break
		}
		start = page.Accounts[len(page.Accounts)-1].Name
	}
	return all, nil
}

// hivedPriceRow flattens a hived base/quote price into the canonical form.
func hivedPriceRow(p hivedPrice) statejsonl.PriceJSON {
	ba, _ := p.Base.AmountInt()
	qa, _ := p.Quote.AmountInt()
	return statejsonl.PriceJSON{
		BaseAmount:  ba,
		BaseSym:     naiToSymU32(p.Base.NAI),
		QuoteAmount: qa,
		QuoteSym:    naiToSymU32(p.Quote.NAI),
	}
}

// curveStrToID reverses curveIDToStr.
func curveStrToID(s string) uint8 {
	switch s {
	case "quadratic":
		return uint8(objects.CurveQuadratic)
	case "bounded_curation", "bounded":
		return uint8(objects.CurveBoundedCurve)
	case "linear":
		return uint8(objects.CurveLinear)
	case "square_root":
		return uint8(objects.CurveSquareRoot)
	case "convergent_linear":
		return uint8(objects.CurveConvergentLinear)
	case "convergent_square_root":
		return uint8(objects.CurveConvergentSquareRoot)
	}
	return 255
}

// naiToSymU32 maps a hived NAI string ("@@000000021" HIVE, "@@000000013"
// HBD, "@@000000037" VESTS) to the legacy asset-num used in our rows.
func naiToSymU32(nai string) uint32 {
	switch nai {
	case "@@000000021":
		return 0xBEBC2023 // HiveSymbol
	case "@@000000013":
		return 0xBEBC2003 // HbdSymbol
	case "@@000000037":
		return 0xBEBC2046 // VestsSymbol
	}
	return 0
}

// looseInt parses an int64 from a decimal string that may be quoted or
// empty (hived returns numbers as both bare and JSON-string forms).
func looseInt(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return 0
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

const hexDigits = "0123456789abcdef"

func hexLower(b []byte) string {
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexDigits[c>>4]
		out[i*2+1] = hexDigits[c&0x0f]
	}
	return string(out)
}

// pubkeyB58ToHex passes through the hived base58 STM key as-is for now;
// the Go side emits a hex of the 33-byte key, so signing_key is in
// snapshot-diff's skip set until a shared encoder lands.
func pubkeyB58ToHex(b58 string) string { return b58 }

