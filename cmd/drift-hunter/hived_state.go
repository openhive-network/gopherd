package main

// Decoded JSON shapes for hived's database_api responses, narrowed to
// the fields we want to compare. We intentionally keep the structs
// loose (json.RawMessage / json.Number / interface{}) because hived's
// JSON shapes drift across hardforks — every comparator pulls only
// the keys it actually reads, so a missing/renamed key on one side
// shows up as a Δ on that field instead of breaking decode.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// hivedDGPO mirrors database_api::api_dynamic_global_property_object.
// The keys present here cover everything the cascade hunting has needed
// so far. Add fields freely as new comparators land.
type hivedDGPO struct {
	HeadBlockNumber               uint32          `json:"head_block_number"`
	HeadBlockID                   string          `json:"head_block_id"`
	Time                          string          `json:"time"`
	CurrentWitness                string          `json:"current_witness"`
	TotalPow                      json.Number     `json:"total_pow"`
	NumPowWitnesses               uint32          `json:"num_pow_witnesses"`
	VirtualSupply                 hivedAsset      `json:"virtual_supply"`
	CurrentSupply                 hivedAsset      `json:"current_supply"`
	CurrentHBDSupply              hivedAsset      `json:"current_hbd_supply"`
	TotalVestingFundHive          hivedAsset      `json:"total_vesting_fund_hive"`
	TotalVestingShares            hivedAsset      `json:"total_vesting_shares"`
	TotalRewardFundHive           hivedAsset      `json:"total_reward_fund_hive"`
	PendingRewardedVestingShares  hivedAsset      `json:"pending_rewarded_vesting_shares"`
	PendingRewardedVestingHive    hivedAsset      `json:"pending_rewarded_vesting_hive"`
	TotalRewardShares2            json.RawMessage `json:"total_reward_shares2"`
	HBDInterestRate               uint16          `json:"hbd_interest_rate"`
	HBDPrintRate                  uint16          `json:"hbd_print_rate"`
	HBDStartPercent               uint16          `json:"hbd_start_percent"`
	HBDStopPercent                uint16          `json:"hbd_stop_percent"`
	ContentRewardPercent          uint16          `json:"content_reward_percent"`
	VestingRewardPercent          uint16          `json:"vesting_reward_percent"`
	ProposalFundPercent           uint16          `json:"proposal_fund_percent"`
	ReverseAuctionSeconds         uint32          `json:"reverse_auction_seconds"`
	DelegationReturnPeriod        uint32          `json:"delegation_return_period"`
	AvailableAccountSubsidies     int64           `json:"available_account_subsidies"`
	LastIrreversibleBlockNum      uint32          `json:"last_irreversible_block_num"`
	DownvotePoolPercent           uint16          `json:"downvote_pool_percent"`
	DHFIntervalLedger             json.RawMessage `json:"dhf_interval_ledger"`
	CurrentAslot                  json.Number     `json:"current_aslot"`
	NextMaintenanceTime           string          `json:"next_maintenance_time"`
	LastBudgetTime                string          `json:"last_budget_time"`
	NextDailyMaintenanceTime      string          `json:"next_daily_maintenance_time"`
	ContentRewardEffectiveSecs    json.RawMessage `json:"content_reward_effective_secs"`
}

// hivedAsset matches `{"amount":"123","precision":3,"nai":"@@000000021"}`.
// The amount is JSON-string for HBD/HIVE post-HF20 (legacy non-NAI form
// is "123.000 HIVE" which we surface as a parse warning if encountered).
type hivedAsset struct {
	Amount    json.RawMessage `json:"amount"`
	Precision uint8           `json:"precision"`
	NAI       string          `json:"nai"`
}

// AmountInt returns the asset amount as an int64. NAI/precision is not
// validated — comparators that care should compare the asset side-by-side
// at the symbol layer.
func (a hivedAsset) AmountInt() (int64, error) {
	if len(a.Amount) == 0 {
		return 0, nil
	}
	// Strip the surrounding quotes (post-HF20 amounts are JSON strings).
	s := string(a.Amount)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	return strconv.ParseInt(s, 10, 64)
}

// fetchDGPO grabs hived's dynamic_global_property_object.
func (c *rpcClient) fetchDGPO() (*hivedDGPO, error) {
	raw, err := c.call("database_api.get_dynamic_global_properties", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var d hivedDGPO
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("decode dgpo: %w", err)
	}
	return &d, nil
}

// hivedWitnessSchedule mirrors api_witness_schedule_object.
type hivedWitnessSchedule struct {
	NumScheduledWitnesses     uint8       `json:"num_scheduled_witnesses"`
	CurrentShuffledWitnesses  []string    `json:"current_shuffled_witnesses"`
	NextShuffleBlockNum       uint32      `json:"next_shuffle_block_num"`
	CurrentVirtualTime        json.Number `json:"current_virtual_time"`
	Top19Weight               uint32      `json:"top19_weight"`
	TimeshareWeight           uint32      `json:"timeshare_weight"`
	MinerWeight               uint32      `json:"miner_weight"`
	WitnessPayNormFactor      uint32      `json:"witness_pay_normalization_factor"`
	HardforkRequiredWitnesses uint8       `json:"hardfork_required_witnesses"`
	MajorityVersion           string      `json:"majority_version"`
	MaxVotedWitnesses         uint8       `json:"max_voted_witnesses"`
	MaxMinerWitnesses         uint8       `json:"max_miner_witnesses"`
	MaxRunnerWitnesses        uint8       `json:"max_runner_witnesses"`
	MedianProps               hivedMedianProps `json:"median_props"`
}

// hivedMedianProps mirrors median_chain_properties decoded from the JSON.
type hivedMedianProps struct {
	AccountCreationFee hivedAsset      `json:"account_creation_fee"`
	MaximumBlockSize   uint32          `json:"maximum_block_size"`
	HBDInterestRate    uint16          `json:"hbd_interest_rate"`
	AccountSubsidyBudget int32         `json:"account_subsidy_budget"`
	AccountSubsidyDecay  uint32        `json:"account_subsidy_decay"`
}

func (c *rpcClient) fetchWitnessSchedule() (*hivedWitnessSchedule, error) {
	raw, err := c.call("database_api.get_witness_schedule", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var s hivedWitnessSchedule
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("decode witness_schedule: %w", err)
	}
	return &s, nil
}

// hivedFeedHistory mirrors api_feed_history_object.
type hivedFeedHistory struct {
	CurrentMedianHistory hivedPrice   `json:"current_median_history"`
	MarketMedianHistory  hivedPrice   `json:"market_median_history"`
	CurrentMinHistory    hivedPrice   `json:"current_min_history"`
	CurrentMaxHistory    hivedPrice   `json:"current_max_history"`
	PriceHistory         []hivedPrice `json:"price_history"`
}

type hivedPrice struct {
	Base  hivedAsset `json:"base"`
	Quote hivedAsset `json:"quote"`
}

func (c *rpcClient) fetchFeedHistory() (*hivedFeedHistory, error) {
	raw, err := c.call("database_api.get_feed_history", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var h hivedFeedHistory
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("decode feed_history: %w", err)
	}
	return &h, nil
}

// hivedRewardFund mirrors api_reward_fund_object.
type hivedRewardFund struct {
	Name                  string          `json:"name"`
	RewardBalance         hivedAsset      `json:"reward_balance"`
	RecentClaims          json.Number     `json:"recent_claims"`
	LastUpdate            string          `json:"last_update"`
	ContentConstant       json.Number     `json:"content_constant"`
	PercentCurationRewards uint16         `json:"percent_curation_rewards"`
	PercentContentRewards  uint16         `json:"percent_content_rewards"`
	AuthorRewardCurve      string         `json:"author_reward_curve"`
	CurationRewardCurve    string         `json:"curation_reward_curve"`
}

func (c *rpcClient) fetchRewardFunds() ([]hivedRewardFund, error) {
	raw, err := c.call("database_api.get_reward_funds", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var page struct {
		Funds []hivedRewardFund `json:"funds"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("decode reward_funds: %w", err)
	}
	return page.Funds, nil
}

// hivedWitness is the partial mirror of api_witness_object. JSON keys
// are the canonical hived names; missing fields decode to zero values.
type hivedWitness struct {
	Owner                            string      `json:"owner"`
	Created                          string      `json:"created"`
	URL                              string      `json:"url"`
	Votes                            json.Number `json:"votes"`
	TotalMissedBlocks                uint32      `json:"total_missed"`
	LastConfirmedBlockNum            uint32      `json:"last_confirmed_block_num"`
	PowWorker                        uint64      `json:"pow_worker"`
	VirtualLastUpdate                json.Number `json:"virtual_last_update"`
	VirtualPosition                  json.Number `json:"virtual_position"`
	VirtualScheduledTime             json.Number `json:"virtual_scheduled_time"`
	HardforkVersionVote              string      `json:"hardfork_version_vote"`
	HardforkTimeVote                 string      `json:"hardfork_time_vote"`
	RunningVersion                   string      `json:"running_version"`
	SigningKey                       string      `json:"signing_key"`
	HBDExchangeRate                  hivedPrice  `json:"hbd_exchange_rate"`
	LastHBDExchangeUpdate            string      `json:"last_hbd_exchange_update"`
	AvailableWitnessAccountSubsidies int64       `json:"available_witness_account_subsidies"`
	Props                            hivedMedianProps `json:"props"`
}

// fetchWitnessesByName pages through database_api.list_witnesses, sorted
// by_name. Returns every witness up to the (high but bounded) cap.
func (c *rpcClient) fetchWitnessesByName(cap int) ([]hivedWitness, error) {
	var all []hivedWitness
	start := ""
	const pageSize = 1000
	for {
		params := map[string]interface{}{
			"start": start,
			"limit": pageSize,
			"order": "by_name",
		}
		raw, err := c.call("database_api.list_witnesses", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Witnesses []hivedWitness `json:"witnesses"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("decode list_witnesses: %w", err)
		}
		if len(page.Witnesses) == 0 {
			break
		}
		if len(all) == 0 {
			all = append(all, page.Witnesses...)
		} else {
			// First entry of subsequent pages is the resume cursor.
			all = append(all, page.Witnesses[1:]...)
		}
		if len(page.Witnesses) < pageSize {
			break
		}
		if cap > 0 && len(all) >= cap {
			break
		}
		start = page.Witnesses[len(page.Witnesses)-1].Owner
	}
	return all, nil
}

// fetchTopWitnessesByVotes pages list_witnesses sorted by vote DESC and
// returns the first `limit` (default 21).
func (c *rpcClient) fetchTopWitnessesByVotes(limit int) ([]hivedWitness, error) {
	if limit <= 0 {
		limit = 21
	}
	params := map[string]interface{}{
		"start": []interface{}{},
		"limit": limit,
		"order": "by_vote_name",
	}
	raw, err := c.call("database_api.list_witnesses", params)
	if err != nil {
		// Fallback: pull everything by_name and sort locally. Slower
		// but works on hiveds where by_vote_name happens to be cranky.
		all, ferr := c.fetchWitnessesByName(0)
		if ferr != nil {
			return nil, fmt.Errorf("list_witnesses: %v (fallback failed: %v)", err, ferr)
		}
		sort.Slice(all, func(i, j int) bool {
			vi, _ := all[i].Votes.Int64()
			vj, _ := all[j].Votes.Int64()
			if vi != vj {
				return vi > vj
			}
			return all[i].Owner < all[j].Owner
		})
		if limit < len(all) {
			all = all[:limit]
		}
		return all, nil
	}
	var page struct {
		Witnesses []hivedWitness `json:"witnesses"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("decode list_witnesses: %w", err)
	}
	return page.Witnesses, nil
}

// hivedAccount mirrors api_account_object — narrowed to balance / vesting
// fields that drive the cryptos-class divergence investigation.
type hivedAccount struct {
	Name                       string     `json:"name"`
	Balance                    hivedAsset `json:"balance"`
	HBDBalance                 hivedAsset `json:"hbd_balance"`
	SavingsBalance             hivedAsset `json:"savings_balance"`
	SavingsHBDBalance          hivedAsset `json:"savings_hbd_balance"`
	VestingShares              hivedAsset `json:"vesting_shares"`
	DelegatedVestingShares     hivedAsset `json:"delegated_vesting_shares"`
	ReceivedVestingShares      hivedAsset `json:"received_vesting_shares"`
	RewardHiveBalance          hivedAsset `json:"reward_hive_balance"`
	RewardHBDBalance           hivedAsset `json:"reward_hbd_balance"`
	RewardVestingBalance       hivedAsset `json:"reward_vesting_balance"`
	PostCount                  uint32     `json:"post_count"`
	PostBandwidth              uint32     `json:"post_bandwidth"`
	LastRootPost               string     `json:"last_root_post"`
	LastPost                   string     `json:"last_post"`
	VestingWithdrawRate        hivedAsset `json:"vesting_withdraw_rate"`
	NextVestingWithdrawal      string     `json:"next_vesting_withdrawal"`
	WithdrawnPerPeriod         json.Number `json:"withdrawn"`
	ToWithdraw                 json.Number `json:"to_withdraw"`
	WithdrawRoutes             uint16     `json:"withdraw_routes"`
}

// fetchAccountByName fetches a single account via database_api.find_accounts.
func (c *rpcClient) fetchAccountByName(name string) (*hivedAccount, error) {
	params := map[string]interface{}{"accounts": []string{name}}
	raw, err := c.call("database_api.find_accounts", params)
	if err != nil {
		return nil, err
	}
	var page struct {
		Accounts []hivedAccount `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("decode find_accounts: %w", err)
	}
	if len(page.Accounts) == 0 {
		return nil, fmt.Errorf("account %q not found", name)
	}
	return &page.Accounts[0], nil
}

// fetchWitnessByName fetches a single witness by primary key.
func (c *rpcClient) fetchWitnessByName(owner string) (*hivedWitness, error) {
	params := map[string]interface{}{"owners": []string{owner}}
	raw, err := c.call("database_api.find_witnesses", params)
	if err != nil {
		return nil, err
	}
	var page struct {
		Witnesses []hivedWitness `json:"witnesses"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("decode find_witnesses: %w", err)
	}
	if len(page.Witnesses) == 0 {
		return nil, fmt.Errorf("witness %q not found", owner)
	}
	return &page.Witnesses[0], nil
}
