// Package statejsonl defines the canonical, deterministic JSONL row
// format used to compare ghive (Go) chain state against hived (C++).
//
// Both producers — `ghive-tools state-jsonl` (from a Go snapshot) and
// `drift-hunter dump-jsonl` (from a hived checkpoint RPC) — emit these
// exact structs, and `ghive-tools snapshot-diff` aligns them with
// IdentKey. Keeping the row types and IdentKey in ONE package means the
// two sides can never disagree on field names or sort order — a silent
// format mismatch would otherwise make every object look "changed".
package statejsonl

import "encoding/json"

// PriceJSON flattens a base/quote price into comparable scalar fields.
type PriceJSON struct {
	BaseAmount  int64  `json:"base_amount"`
	BaseSym     uint32 `json:"base_sym"`
	QuoteAmount int64  `json:"quote_amount"`
	QuoteSym    uint32 `json:"quote_sym"`
}

// DGPORow is the dynamic global property object.
type DGPORow struct {
	Type                         string `json:"_type"`
	HeadBlockNumber              uint32 `json:"head_block_number"`
	HeadBlockID                  string `json:"head_block_id"`
	Time                         uint32 `json:"time"`
	CurrentWitness               string `json:"current_witness"`
	CurrentSupply                int64  `json:"current_supply"`
	CurrentHBDSupply             int64  `json:"current_hbd_supply"`
	TotalVestingShares           int64  `json:"total_vesting_shares"`
	TotalVestingFundHive         int64  `json:"total_vesting_fund_hive"`
	TotalRewardFundHive          int64  `json:"total_reward_fund_hive"`
	VirtualSupply                int64  `json:"virtual_supply"`
	PendingRewardedVestingShares int64  `json:"pending_rewarded_vesting_shares"`
	PendingRewardedVestingHive   int64  `json:"pending_rewarded_vesting_hive"`
	DHFIntervalLedger            int64  `json:"dhf_interval_ledger"`
	HBDInterestRate              uint16 `json:"hbd_interest_rate"`
	HBDPrintRate                 uint16 `json:"hbd_print_rate"`
	HBDStartPercent              uint16 `json:"hbd_start_percent"`
	HBDStopPercent               uint16 `json:"hbd_stop_percent"`
	ContentRewardPercent         uint16 `json:"content_reward_percent"`
	VestingRewardPercent         uint16 `json:"vesting_reward_percent"`
	ProposalFundPercent          uint16 `json:"proposal_fund_percent"`
	ReverseAuctionSeconds        uint32 `json:"reverse_auction_seconds"`
	DelegationReturnPeriod       uint32 `json:"delegation_return_period"`
	AvailableAccountSubsidies    int64  `json:"available_account_subsidies"`
}

// FeedHistoryRow is the price-feed singleton.
type FeedHistoryRow struct {
	Type             string    `json:"_type"`
	CurrentMedian    PriceJSON `json:"current_median"`
	MarketMedian     PriceJSON `json:"market_median"`
	CurrentMin       PriceJSON `json:"current_min"`
	CurrentMax       PriceJSON `json:"current_max"`
	PriceHistoryLen  int       `json:"price_history_len"`
	PriceHistoryLast PriceJSON `json:"price_history_last"`
}

// AccountRow is an account_object. Fields that hived exposes but the Go
// port doesn't compare cleanly (RC/voting mana regen, interest stamps)
// are present so a Go-vs-Go diff is total; snapshot-diff's --skip-fields
// drops the legitimately-divergent ones for Go-vs-hived runs.
type AccountRow struct {
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
	PendingClaimedAccounts        int64  `json:"pending_claimed_accounts"`
	RecoveryAccount               string `json:"recovery_account"`
	Mined                         bool   `json:"mined"`
	CanVote                       bool   `json:"can_vote"`
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

// WitnessRow is a witness_object.
type WitnessRow struct {
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

// RewardFundRow is a reward_fund_object.
type RewardFundRow struct {
	Type                  string `json:"_type"`
	Name                  string `json:"name"`
	RewardBalance         int64  `json:"reward_balance"`
	RecentClaims          string `json:"recent_claims"`
	LastUpdate            uint32 `json:"last_update"`
	ContentConstant       int64  `json:"content_constant"`
	PercentCuration       uint16 `json:"percent_curation_rewards"`
	PercentContent        uint16 `json:"percent_content_rewards"`
	AuthorRewardCurve     uint8  `json:"author_reward_curve"`
	CurationRewardCurve   uint8  `json:"curation_reward_curve"`
	DownvoteRewardBalance int64  `json:"downvote_reward_balance"`
}

// IdentKey extracts the type+identifier from a JSONL row, used to both
// SORT the file (the writer) and ALIGN the two files (the differ).
// Numeric ID components are zero-padded so lexical order equals numeric
// order ("order/9" must sort before "order/10").
func IdentKey(line []byte) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return string(line)
	}
	t := Unquote(m["_type"])
	f := func(k string) string { return Unquote(m[k]) }
	switch t {
	case "account":
		return "account/" + f("name")
	case "witness":
		return "witness/" + f("owner")
	case "reward_fund":
		return "reward_fund/" + f("name")
	case "comment":
		return "comment/" + f("author") + "/" + f("permlink")
	case "limit_order":
		return "limit_order/" + f("owner") + "/" + padNum(f("order_id"))
	case "escrow":
		return "escrow/" + f("from") + "/" + padNum(f("escrow_id"))
	case "convert_request":
		return "convert_request/" + f("owner") + "/" + padNum(f("request_id"))
	case "collateralized_convert":
		return "collateralized_convert/" + f("owner") + "/" + padNum(f("request_id"))
	case "savings_withdraw":
		return "savings_withdraw/" + f("from") + "/" + padNum(f("request_id"))
	case "vesting_delegation_expiration":
		return "vesting_delegation_expiration/" + f("delegator") + "/" + padNum(f("id"))
	case "proposal":
		return "proposal/" + padNum(f("id"))
	default:
		// Singletons (dgpo, feed_history) and any future bare type.
		return t
	}
}

// Unquote strips surrounding double quotes from a JSON scalar.
func Unquote(b json.RawMessage) string {
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		return string(b[1 : len(b)-1])
	}
	return string(b)
}

// padNum left-pads a decimal string to 20 chars. Non-numeric input is
// returned unchanged.
func padNum(s string) string {
	if s == "" {
		return s
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return s
		}
	}
	if len(s) >= 20 {
		return s
	}
	return "00000000000000000000"[:20-len(s)] + s
}
