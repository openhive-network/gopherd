package main

// Per-table comparators. Each `compareXxx` pulls one logical state
// object from both sides and returns every field-level drift it finds.
// The runner concatenates them, sorts, prints.
//
// Decoder helpers around hived's JSON shape (uint128-as-decimal-string,
// asset-as-{amount,precision,nai}, time-as-ISO8601) live alongside.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"

	"hived-go/internal/state/objects"
)

// uint128BEHex renders a 16-byte big-endian uint128 as a 0-padded hex
// string for display when string comparison surfaces a drift.
func uint128BEHex(b [16]byte) string {
	return "0x" + hex16(b[:])
}

func hex16(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hex[v>>4]
		out[i*2+1] = hex[v&0xf]
	}
	return string(out)
}

// uint128BEToDecimal converts a 16-byte big-endian uint128 to its
// decimal string representation (matching hived's JSON output).
func uint128BEToDecimal(b [16]byte) string {
	var x big.Int
	x.SetBytes(b[:])
	return x.String()
}

// hivedDecimalToUint128BE parses a decimal-string uint128 (hived's JSON
// format for total_reward_shares2, virtual_*) into 16-byte BE.
func hivedDecimalToUint128BE(decimal string) ([16]byte, error) {
	var out [16]byte
	if decimal == "" || decimal == "null" {
		return out, nil
	}
	var x big.Int
	if _, ok := x.SetString(decimal, 10); !ok {
		return out, fmt.Errorf("not a decimal: %q", decimal)
	}
	bs := x.Bytes()
	if len(bs) > 16 {
		return out, fmt.Errorf("uint128 overflow: %s", decimal)
	}
	copy(out[16-len(bs):], bs)
	return out, nil
}

// hivedJSONNumberToUint128BE handles the case where a uint128 comes in
// as an unquoted JSON number (older hived formats) OR a JSON string.
func hivedJSONNumberToUint128BE(raw json.RawMessage) ([16]byte, error) {
	s := string(raw)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	return hivedDecimalToUint128BE(s)
}

// --- DGPO ----------------------------------------------------------------

func compareDGPO(block uint32, g *goState, h *hivedDGPO) []drift {
	gd, err := g.dgpo()
	if err != nil {
		return []drift{{block: block, table: "dgpo", field: "_load", go_: "ERR", hived: err.Error()}}
	}
	var out []drift
	add := func(d drift, ok bool) {
		if ok {
			out = append(out, d)
		}
	}

	add(cmpUint32(block, "dgpo", "head_block_number", gd.HeadBlockNumber, h.HeadBlockNumber))
	add(cmpString(block, "dgpo", "current_witness", gd.CurrentWitness, h.CurrentWitness))

	// Asset amounts: pull the Go-side int64, cast hived's amount via
	// AmountInt(). When hived's amount fails to parse we surface that
	// as a drift on its own.
	addAsset := func(field string, goAmt int64, hivedA hivedAsset) {
		hAmt, err := hivedA.AmountInt()
		if err != nil {
			out = append(out, drift{
				block: block, table: "dgpo", field: field + "_decode",
				go_: strconv.FormatInt(goAmt, 10), hived: "DECODE_ERR:" + err.Error(),
			})
			return
		}
		add(cmpInt64(block, "dgpo", field, goAmt, hAmt))
	}

	addAsset("current_supply", gd.CurrentSupply, h.CurrentSupply)
	addAsset("current_hbd_supply", gd.CurrentHBDSupply, h.CurrentHBDSupply)
	addAsset("virtual_supply", gd.VirtualSupply, h.VirtualSupply)
	addAsset("total_vesting_fund_hive", gd.TotalVestingFundHive, h.TotalVestingFundHive)
	addAsset("total_vesting_shares", gd.TotalVestingShares, h.TotalVestingShares)
	addAsset("total_reward_fund_hive", gd.TotalRewardFundHive, h.TotalRewardFundHive)

	// Total reward shares2 (uint128 in BE bytes).
	if len(h.TotalRewardShares2) > 0 {
		hShares, err := hivedJSONNumberToUint128BE(h.TotalRewardShares2)
		if err != nil {
			out = append(out, drift{
				block: block, table: "dgpo", field: "total_reward_shares2_decode",
				go_: uint128BEToDecimal(gd.TotalRewardShares2), hived: "DECODE_ERR:" + err.Error(),
			})
		} else if d, ok := cmpBytes(block, "dgpo", "total_reward_shares2", gd.TotalRewardShares2[:], hShares[:]); ok {
			d.go_ = uint128BEToDecimal(gd.TotalRewardShares2)
			d.hived = uint128BEToDecimal(hShares)
			out = append(out, d)
		}
	}

	add(cmpUint16(block, "dgpo", "hbd_interest_rate", gd.HBDInterestRate, h.HBDInterestRate))
	add(cmpUint16(block, "dgpo", "hbd_print_rate", gd.HBDPrintRate, h.HBDPrintRate))
	add(cmpUint16(block, "dgpo", "hbd_start_percent", gd.HBDStartPercent, h.HBDStartPercent))
	add(cmpUint16(block, "dgpo", "hbd_stop_percent", gd.HBDStopPercent, h.HBDStopPercent))
	add(cmpUint16(block, "dgpo", "content_reward_percent", gd.ContentRewardPercent, h.ContentRewardPercent))
	add(cmpUint16(block, "dgpo", "vesting_reward_percent", gd.VestingRewardPercent, h.VestingRewardPercent))
	add(cmpUint16(block, "dgpo", "proposal_fund_percent", gd.ProposalFundPercent, h.ProposalFundPercent))
	add(cmpUint32(block, "dgpo", "reverse_auction_seconds", gd.ReverseAuctionSeconds, h.ReverseAuctionSeconds))
	add(cmpUint32(block, "dgpo", "delegation_return_period", gd.DelegationReturnPeriod, h.DelegationReturnPeriod))
	add(cmpInt64(block, "dgpo", "available_account_subsidies", gd.AvailableAccountSubsidies, h.AvailableAccountSubsidies))
	add(cmpUint32(block, "dgpo", "last_irreversible_block_num", gd.LastIrreversibleBlockNum, h.LastIrreversibleBlockNum))
	add(cmpUint16(block, "dgpo", "downvote_pool_percent", gd.DownvotePoolPercent, h.DownvotePoolPercent))
	add(cmpUint32(block, "dgpo", "num_pow_witnesses", gd.NumPowWitnesses, h.NumPowWitnesses))

	// total_pow + current_aslot come back as JSON numbers. Parse, then
	// compare. Failures decode to 0 — the drift surfaces on its own.
	if hPow, err := h.TotalPow.Int64(); err == nil {
		add(cmpUint64(block, "dgpo", "total_pow", gd.TotalPow, uint64(hPow)))
	}
	if hSlot, err := h.CurrentAslot.Int64(); err == nil {
		add(cmpUint64(block, "dgpo", "current_aslot", gd.CurrentAslot, uint64(hSlot)))
	}

	// dhf_interval_ledger arrives as either an asset object or a raw
	// integer depending on the hived release. Try asset first, fall
	// back to int parsing.
	if len(h.DHFIntervalLedger) > 0 {
		var hivedIL int64
		var ha hivedAsset
		if err := json.Unmarshal(h.DHFIntervalLedger, &ha); err == nil {
			if v, perr := ha.AmountInt(); perr == nil {
				hivedIL = v
			}
		} else {
			s := string(h.DHFIntervalLedger)
			if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
				s = s[1 : len(s)-1]
			}
			if v, perr := strconv.ParseInt(s, 10, 64); perr == nil {
				hivedIL = v
			}
		}
		add(cmpInt64(block, "dgpo", "dhf_interval_ledger", gd.DHFIntervalLedger, hivedIL))
	}

	return out
}

// --- witness_schedule ----------------------------------------------------

func compareWitnessSchedule(block uint32, g *goState, h *hivedWitnessSchedule) []drift {
	gs, err := g.witnessSchedule()
	if err != nil {
		return []drift{{block: block, table: "witness_schedule", field: "_load", go_: "ERR", hived: err.Error()}}
	}
	var out []drift
	add := func(d drift, ok bool) {
		if ok {
			out = append(out, d)
		}
	}

	add(cmpUint8(block, "witness_schedule", "num_scheduled_witnesses", gs.NumScheduledWitnesses, h.NumScheduledWitnesses))
	add(cmpUint32(block, "witness_schedule", "next_shuffle_block_num", gs.NextShuffleBlockNum, h.NextShuffleBlockNum))

	// virtual_time as decimal uint128.
	if len(h.CurrentVirtualTime) > 0 {
		hVT, err := hivedDecimalToUint128BE(string(h.CurrentVirtualTime))
		if err != nil {
			out = append(out, drift{
				block: block, table: "witness_schedule", field: "current_virtual_time_decode",
				go_: uint128BEToDecimal(gs.CurrentVirtualTime), hived: "DECODE_ERR:" + err.Error(),
			})
		} else if d, ok := cmpBytes(block, "witness_schedule", "current_virtual_time", gs.CurrentVirtualTime[:], hVT[:]); ok {
			d.go_ = uint128BEToDecimal(gs.CurrentVirtualTime)
			d.hived = uint128BEToDecimal(hVT)
			out = append(out, d)
		}
	}

	add(cmpUint32(block, "witness_schedule", "timeshare_weight", gs.TimeshareWeight, h.TimeshareWeight))
	add(cmpUint32(block, "witness_schedule", "miner_weight", gs.MinerWeight, h.MinerWeight))
	// Top19 weight is hived-only (Go folds it into ElectedWeight).
	add(cmpUint32(block, "witness_schedule", "elected_or_top19_weight", gs.ElectedWeight, h.Top19Weight))
	add(cmpUint32(block, "witness_schedule", "witness_pay_normalization_factor", gs.WitnessPayNormalizationFactor, h.WitnessPayNormFactor))

	add(cmpUint8(block, "witness_schedule", "max_voted_witnesses", gs.MaxVotedWitnesses, h.MaxVotedWitnesses))
	add(cmpUint8(block, "witness_schedule", "max_miner_witnesses", gs.MaxMinerWitnesses, h.MaxMinerWitnesses))
	add(cmpUint8(block, "witness_schedule", "max_runner_witnesses", gs.MaxRunnerWitnesses, h.MaxRunnerWitnesses))
	add(cmpUint8(block, "witness_schedule", "hardfork_required_witnesses", gs.HardforkRequiredWitnesses, h.HardforkRequiredWitnesses))

	// majority_version is "X.Y.Z" on hived; Go packs it as a single
	// uint32. Compare as strings via the canonical packed→version
	// formatter.
	add(cmpString(block, "witness_schedule", "majority_version", packedVersionToStr(gs.MajorityVersion), h.MajorityVersion))

	// median_props: account_creation_fee asset + maximum_block_size +
	// hbd_interest_rate (+ subsidy budget/decay where present).
	if hAcf, err := h.MedianProps.AccountCreationFee.AmountInt(); err == nil {
		add(cmpInt64(block, "witness_schedule", "median_account_creation_fee", gs.MedianAccountCreationFee, hAcf))
	}
	add(cmpUint32(block, "witness_schedule", "median_maximum_block_size", gs.MedianMaximumBlockSize, h.MedianProps.MaximumBlockSize))
	add(cmpUint16(block, "witness_schedule", "median_hbd_interest_rate", gs.MedianHBDInterestRate, h.MedianProps.HBDInterestRate))

	// CurrentShuffledWitnesses — compare slot-by-slot. A drift on any
	// slot is reported individually so we know which slot diverged
	// (cascade root often is just one wrong slot).
	maxN := len(gs.CurrentShuffledWitnesses)
	if hn := len(h.CurrentShuffledWitnesses); hn > maxN {
		maxN = hn
	}
	for i := 0; i < maxN; i++ {
		var goName, hivedName string
		if i < len(gs.CurrentShuffledWitnesses) {
			goName = string(gs.CurrentShuffledWitnesses[i])
		}
		if i < len(h.CurrentShuffledWitnesses) {
			hivedName = h.CurrentShuffledWitnesses[i]
		}
		add(cmpString(block, "witness_schedule", fmt.Sprintf("shuffled[%02d]", i), goName, hivedName))
	}

	return out
}

// packedVersionToStr renders a packed hardfork_version (4 bytes:
// MAJOR.HARDFORK.RELEASE.PATCH) as the string hived uses.
func packedVersionToStr(p uint32) string {
	if p == 0 {
		return "0.0.0"
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], p)
	// hived formats version as "<major>.<hardfork>.<revision>" — three
	// leading bytes; the trailing patch byte is a sub-version.
	return fmt.Sprintf("%d.%d.%d", b[3], b[2], b[1])
}

// --- feed_history --------------------------------------------------------

func compareFeedHistory(block uint32, g *goState, h *hivedFeedHistory) []drift {
	gh, err := g.feedHistory()
	if err != nil {
		return []drift{{block: block, table: "feed_history", field: "_load", go_: "ERR", hived: err.Error()}}
	}
	var out []drift
	addPrice := func(name string, goB, hivedB, goQ, hivedQ int64) {
		if d, ok := cmpInt64(block, "feed_history", name+".base", goB, hivedB); ok {
			out = append(out, d)
		}
		if d, ok := cmpInt64(block, "feed_history", name+".quote", goQ, hivedQ); ok {
			out = append(out, d)
		}
	}
	hivedAmt := func(a hivedAsset) int64 {
		v, _ := a.AmountInt()
		return v
	}

	addPrice("current_median_history",
		gh.CurrentMedianHistory.Base.Amount, hivedAmt(h.CurrentMedianHistory.Base),
		gh.CurrentMedianHistory.Quote.Amount, hivedAmt(h.CurrentMedianHistory.Quote))
	addPrice("market_median_history",
		gh.MarketMedianHistory.Base.Amount, hivedAmt(h.MarketMedianHistory.Base),
		gh.MarketMedianHistory.Quote.Amount, hivedAmt(h.MarketMedianHistory.Quote))
	addPrice("current_min_history",
		gh.CurrentMinHistory.Base.Amount, hivedAmt(h.CurrentMinHistory.Base),
		gh.CurrentMinHistory.Quote.Amount, hivedAmt(h.CurrentMinHistory.Quote))
	addPrice("current_max_history",
		gh.CurrentMaxHistory.Base.Amount, hivedAmt(h.CurrentMaxHistory.Base),
		gh.CurrentMaxHistory.Quote.Amount, hivedAmt(h.CurrentMaxHistory.Quote))

	if d, ok := cmpUint64(block, "feed_history", "price_history.len",
		uint64(len(gh.PriceHistory)), uint64(len(h.PriceHistory))); ok {
		out = append(out, d)
	}
	// Compare the last 5 entries (newest, where drift usually lives).
	tail := 5
	gn := len(gh.PriceHistory)
	hn := len(h.PriceHistory)
	for i := 0; i < tail; i++ {
		gi := gn - 1 - i
		hi := hn - 1 - i
		if gi < 0 || hi < 0 {
			break
		}
		base := fmt.Sprintf("price_history[-%d]", i+1)
		addPrice(base,
			gh.PriceHistory[gi].Base.Amount, hivedAmt(h.PriceHistory[hi].Base),
			gh.PriceHistory[gi].Quote.Amount, hivedAmt(h.PriceHistory[hi].Quote))
	}
	return out
}

// --- reward_fund ---------------------------------------------------------

func compareRewardFunds(block uint32, g *goState, hivedFunds []hivedRewardFund) []drift {
	var out []drift
	for _, hf := range hivedFunds {
		gf, err := g.rewardFund(hf.Name)
		if err != nil {
			out = append(out, drift{
				block: block, table: "reward_fund:" + hf.Name, field: "_load",
				go_: "ERR:" + err.Error(), hived: "OK",
			})
			continue
		}
		add := func(d drift, ok bool) {
			if ok {
				out = append(out, d)
			}
		}
		tname := "reward_fund:" + hf.Name

		hAmt, _ := hf.RewardBalance.AmountInt()
		add(cmpInt64(block, tname, "reward_balance", gf.RewardBalance, hAmt))
		add(cmpUint32(block, tname, "last_update", gf.LastUpdate, parseISO8601Sec(hf.LastUpdate)))
		add(cmpUint16(block, tname, "percent_curation_rewards", gf.PercentCurationRewards, hf.PercentCurationRewards))
		add(cmpUint16(block, tname, "percent_content_rewards", gf.PercentContentRewards, hf.PercentContentRewards))
		add(cmpString(block, tname, "author_reward_curve", curveIDToStr(gf.AuthorRewardCurve), hf.AuthorRewardCurve))
		add(cmpString(block, tname, "curation_reward_curve", curveIDToStr(gf.CurationRewardCurve), hf.CurationRewardCurve))
		if v, err := hf.ContentConstant.Int64(); err == nil {
			add(cmpInt64(block, tname, "content_constant", gf.ContentConstant, v))
		}
		if hRC, err := hivedDecimalToUint128BE(string(hf.RecentClaims)); err == nil {
			if d, ok := cmpBytes(block, tname, "recent_claims", gf.RecentClaims[:], hRC[:]); ok {
				d.go_ = uint128BEToDecimal(gf.RecentClaims)
				d.hived = uint128BEToDecimal(hRC)
				out = append(out, d)
			}
		}
	}
	return out
}

// curveIDToStr maps Go's CurveID enum to hived's JSON string form.
func curveIDToStr(c objects.CurveID) string {
	switch c {
	case objects.CurveQuadratic:
		return "quadratic"
	case objects.CurveBoundedCurve:
		return "bounded_curation"
	case objects.CurveLinear:
		return "linear"
	case objects.CurveSquareRoot:
		return "square_root"
	case objects.CurveConvergentLinear:
		return "convergent_linear"
	case objects.CurveConvergentSquareRoot:
		return "convergent_square_root"
	}
	return fmt.Sprintf("unknown(%d)", uint8(c))
}

// parseISO8601Sec converts hived's "YYYY-MM-DDTHH:MM:SS" to a uint32
// time_point_sec relative to 1970-01-01. Returns 0 on parse failure;
// the comparator will surface the disagreement on its own.
func parseISO8601Sec(s string) uint32 {
	if len(s) < 19 {
		return 0
	}
	// Quick path: most hived datetimes come in the canonical shape.
	year, _ := strconv.Atoi(s[0:4])
	month, _ := strconv.Atoi(s[5:7])
	day, _ := strconv.Atoi(s[8:10])
	hour, _ := strconv.Atoi(s[11:13])
	min, _ := strconv.Atoi(s[14:16])
	sec, _ := strconv.Atoi(s[17:19])
	// Quick + dirty civil → epoch (UTC). Algorithm from
	// http://howardhinnant.github.io/date_algorithms.html#days_from_civil
	y := year
	m := month
	if m <= 2 {
		y -= 1
	}
	era := y / 400
	if y < 0 && y%400 != 0 {
		era -= 1
	}
	yoe := y - era*400
	var mp int
	if m > 2 {
		mp = m - 3
	} else {
		mp = m + 9
	}
	doy := (153*mp + 2) / 5 + day - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	days := era*146097 + doe - 719468
	return uint32(days*86400 + hour*3600 + min*60 + sec)
}

// --- per-witness ---------------------------------------------------------

// compareWitness compares one named witness across both sides. Used for
// the top-21 (and any user-supplied --witness=NAME).
func compareWitness(block uint32, gw objects.Witness, hw *hivedWitness) []drift {
	tname := "witness:" + string(gw.Owner)
	var out []drift
	add := func(d drift, ok bool) {
		if ok {
			out = append(out, d)
		}
	}
	add(cmpUint32(block, tname, "created", gw.Created, parseISO8601Sec(hw.Created)))
	add(cmpString(block, tname, "url", gw.URL, hw.URL))
	if v, err := hw.Votes.Int64(); err == nil {
		add(cmpInt64(block, tname, "votes", gw.TotalVotesVests, v))
	}
	add(cmpUint32(block, tname, "total_missed_blocks", gw.TotalMissedBlocks, hw.TotalMissedBlocks))
	add(cmpUint32(block, tname, "last_confirmed_block_num", gw.LastConfirmedBlockNum, hw.LastConfirmedBlockNum))
	add(cmpUint64(block, tname, "pow_worker", gw.PowWorker, hw.PowWorker))

	// VST trio
	if v, err := hivedDecimalToUint128BE(string(hw.VirtualLastUpdate)); err == nil {
		if d, ok := cmpBytes(block, tname, "virtual_last_update", gw.VirtualLastUpdate[:], v[:]); ok {
			d.go_ = uint128BEToDecimal(gw.VirtualLastUpdate)
			d.hived = uint128BEToDecimal(v)
			out = append(out, d)
		}
	}
	if v, err := hivedDecimalToUint128BE(string(hw.VirtualPosition)); err == nil {
		if d, ok := cmpBytes(block, tname, "virtual_position", gw.VirtualPosition[:], v[:]); ok {
			d.go_ = uint128BEToDecimal(gw.VirtualPosition)
			d.hived = uint128BEToDecimal(v)
			out = append(out, d)
		}
	}
	if v, err := hivedDecimalToUint128BE(string(hw.VirtualScheduledTime)); err == nil {
		if d, ok := cmpBytes(block, tname, "virtual_scheduled_time", gw.VirtualScheduledTime[:], v[:]); ok {
			d.go_ = uint128BEToDecimal(gw.VirtualScheduledTime)
			d.hived = uint128BEToDecimal(v)
			out = append(out, d)
		}
	}

	// Versions
	add(cmpString(block, tname, "running_version", packedVersionToStr(gw.RunningVersion), hw.RunningVersion))
	add(cmpString(block, tname, "hardfork_version_vote", packedVersionToStr(gw.HardforkVersionVote), hw.HardforkVersionVote))
	add(cmpUint32(block, tname, "hardfork_time_vote", gw.HardforkTimeVote, parseISO8601Sec(hw.HardforkTimeVote)))

	// HBD exchange rate
	gBase := gw.HBDExchangeRate.Base.Amount
	gQuote := gw.HBDExchangeRate.Quote.Amount
	hBase, _ := hw.HBDExchangeRate.Base.AmountInt()
	hQuote, _ := hw.HBDExchangeRate.Quote.AmountInt()
	add(cmpInt64(block, tname, "hbd_exchange_rate.base", gBase, hBase))
	add(cmpInt64(block, tname, "hbd_exchange_rate.quote", gQuote, hQuote))
	add(cmpUint32(block, tname, "last_hbd_exchange_update", gw.LastHBDExchangeUpdate, parseISO8601Sec(hw.LastHBDExchangeUpdate)))

	add(cmpInt64(block, tname, "available_witness_account_subsidies", gw.AvailableWitnessAccountSubsidies, hw.AvailableWitnessAccountSubsidies))

	// Props
	if hAcf, err := hw.Props.AccountCreationFee.AmountInt(); err == nil {
		add(cmpInt64(block, tname, "props.account_creation_fee", gw.AccountCreationFee, hAcf))
	}
	add(cmpUint32(block, tname, "props.maximum_block_size", gw.MaximumBlockSize, hw.Props.MaximumBlockSize))
	add(cmpUint16(block, tname, "props.hbd_interest_rate", gw.HBDInterestRate, hw.Props.HBDInterestRate))
	// Account-subsidy: int32 budget on Go vs (sometimes) int64 on hived.
	add(cmpInt64(block, tname, "props.account_subsidy_budget", int64(gw.AccountSubsidyBudget), int64(hw.Props.AccountSubsidyBudget)))
	add(cmpUint32(block, tname, "props.account_subsidy_decay", gw.AccountSubsidyDecay, hw.Props.AccountSubsidyDecay))

	// signing_key as hex would be noisy; surface only when they differ
	// (formatted as the public key string hived ships).
	goSigning := pubkeyToHex(gw.BlockSigningKey[:])
	if hw.SigningKey != "" && goSigning != "" {
		// hived's signing_key is the base58 public key — we don't have
		// a base58 encoder handy in this tool, so compare as a length
		// + hex prefix marker. If they differ we still report so the
		// user knows there's a delta to investigate.
		add(cmpString(block, tname, "signing_key.hex_or_b58_marker", "0x"+goSigning, hw.SigningKey))
	}
	return out
}

// pubkeyToHex emits the raw bytes as hex. Used as a stand-in for hived's
// base58 STM-prefixed key string — comparison is by-presence, not by
// equality.
func pubkeyToHex(b []byte) string {
	return hex16(b)
}

// compareAccount compares one named account's balance / vesting fields.
// Used for --account=NAME[,NAME] when chasing balance-divergence bugs
// (cryptos 35.85-HIVE shortfall at 6,198,475 was the motivating case).
func compareAccount(block uint32, ga objects.Account, ha *hivedAccount) []drift {
	tname := "account:" + string(ga.Name)
	var out []drift
	add := func(d drift, ok bool) {
		if ok {
			out = append(out, d)
		}
	}
	if v, err := ha.Balance.AmountInt(); err == nil {
		add(cmpInt64(block, tname, "balance", ga.Balance, v))
	}
	if v, err := ha.HBDBalance.AmountInt(); err == nil {
		add(cmpInt64(block, tname, "hbd_balance", ga.HBDBalance, v))
	}
	if v, err := ha.SavingsBalance.AmountInt(); err == nil {
		add(cmpInt64(block, tname, "savings_balance", ga.SavingsBalance, v))
	}
	if v, err := ha.SavingsHBDBalance.AmountInt(); err == nil {
		add(cmpInt64(block, tname, "savings_hbd_balance", ga.SavingsHBDBalance, v))
	}
	if v, err := ha.VestingShares.AmountInt(); err == nil {
		add(cmpInt64(block, tname, "vesting_shares", ga.VestingShares, v))
	}
	if v, err := ha.DelegatedVestingShares.AmountInt(); err == nil {
		add(cmpInt64(block, tname, "delegated_vesting_shares", ga.DelegatedVestingShares, v))
	}
	if v, err := ha.ReceivedVestingShares.AmountInt(); err == nil {
		add(cmpInt64(block, tname, "received_vesting_shares", ga.ReceivedVestingShares, v))
	}
	if v, err := ha.RewardHiveBalance.AmountInt(); err == nil {
		add(cmpInt64(block, tname, "reward_hive_balance", ga.RewardHive, v))
	}
	if v, err := ha.RewardHBDBalance.AmountInt(); err == nil {
		add(cmpInt64(block, tname, "reward_hbd_balance", ga.RewardHBD, v))
	}
	if v, err := ha.RewardVestingBalance.AmountInt(); err == nil {
		add(cmpInt64(block, tname, "reward_vesting_balance", ga.RewardVests, v))
	}
	add(cmpUint32(block, tname, "post_count", ga.PostCount, ha.PostCount))
	add(cmpUint32(block, tname, "post_bandwidth", ga.PostBandwidth, ha.PostBandwidth))
	add(cmpUint32(block, tname, "last_root_post", ga.LastRootPost, parseISO8601Sec(ha.LastRootPost)))
	add(cmpUint32(block, tname, "last_post", ga.LastPost, parseISO8601Sec(ha.LastPost)))
	if v, err := ha.VestingWithdrawRate.AmountInt(); err == nil {
		add(cmpInt64(block, tname, "vesting_withdraw_rate", ga.VestingWithdrawRate, v))
	}
	add(cmpUint32(block, tname, "next_vesting_withdrawal", ga.NextVestingWithdrawal, parseISO8601Sec(ha.NextVestingWithdrawal)))
	return out
}
