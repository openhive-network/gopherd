package api

import (
	"context"
	"encoding/json"

	"hived-go/internal/api/jsonrpc"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

func (s *Server) registerDatabaseAPI() {
	s.reg.Register("database_api.get_dynamic_global_properties", s.mGetDGPO(false))
	s.reg.Register("database_api.get_version", s.mGetVersion())
	s.reg.Register("database_api.get_feed_history", s.mGetFeedHistory(false))
	s.reg.Register("database_api.get_current_price_feed", s.mGetCurrentPriceFeed(false))
	s.reg.Register("database_api.get_witness_schedule", s.mGetWitnessSchedule())
	s.reg.Register("database_api.get_active_witnesses", s.mGetActiveWitnesses())
	s.reg.Register("database_api.get_reward_funds", s.mGetRewardFunds(false))
	s.reg.Register("database_api.get_hardfork_properties", s.mGetHardforkProps())
	s.reg.Register("database_api.find_accounts", s.mFindAccounts(false))
	s.reg.Register("database_api.find_witnesses", s.mFindWitnesses(false))
	s.reg.Register("database_api.get_config", s.mGetConfig())
	s.reg.Register("condenser_api.get_chain_properties", s.mGetChainProperties(true))
	s.reg.Register("condenser_api.get_next_scheduled_hardfork", s.mGetNextScheduledHardfork())
}

// mGetChainProperties returns the median witness chain properties.
func (s *Server) mGetChainProperties(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		var out map[string]interface{}
		if err := s.chain.WithReadSession(func(sess *state.Session) error {
			ws, e := objects.GetWitnessSchedule(sess)
			if e != nil {
				return e
			}
			fee := interface{}(hiveAsset(ws.MedianAccountCreationFee))
			if legacy {
				fee = legacyAsset(hiveAsset(ws.MedianAccountCreationFee))
			}
			out = map[string]interface{}{
				"account_creation_fee": fee,
				"maximum_block_size":   ws.MedianMaximumBlockSize,
				"hbd_interest_rate":    ws.MedianHBDInterestRate,
			}
			return nil
		}); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "get_chain_properties: %v", err)
		}
		return out, nil
	}
}

// mGetNextScheduledHardfork returns the pending hardfork version + live time.
func (s *Server) mGetNextScheduledHardfork() jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		var out map[string]interface{}
		if err := s.chain.WithReadSession(func(sess *state.Session) error {
			hp, e := objects.GetHardforkPropertyObject(sess)
			if e != nil {
				return e
			}
			out = map[string]interface{}{
				"hf_version": "1." + itoa(hp.NextHardfork) + ".0",
				"live_time":  timeString(hp.NextHardforkTime),
			}
			return nil
		}); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "get_next_scheduled_hardfork: %v", err)
		}
		return out, nil
	}
}

// mGetConfig returns the commonly-read chain constants. (hived returns ~100;
// this is the subset apps actually read; extend as needed.)
func (s *Server) mGetConfig() jsonrpc.Handler {
	cfg := map[string]interface{}{
		"HIVE_CHAIN_ID":                  networkChainIDHex(),
		"HIVE_ADDRESS_PREFIX":            "STM",
		"HIVE_BLOCK_INTERVAL":            3,
		"HIVE_BLOCKS_PER_YEAR":           uint32(60 * 60 * 24 * 365 / 3),
		"HIVE_BLOCKS_PER_DAY":            uint32(60 * 60 * 24 / 3),
		"HIVE_MAX_WITNESSES":             21,
		"HIVE_MAX_VOTED_WITNESSES_HF17":  20,
		"HIVE_MAX_RUNNER_WITNESSES_HF17": 1,
		"HIVE_HARDFORK_REQUIRED_WITNESSES": 17,
		"HIVE_100_PERCENT":               10000,
		"HIVE_1_PERCENT":                 100,
		"HIVE_MAX_TIME_UNTIL_EXPIRATION": 3600,
		"HIVE_MIN_BLOCK_SIZE_LIMIT":      65536,
		"HIVE_IRREVERSIBLE_THRESHOLD":    7500,
		"HIVE_VESTING_WITHDRAW_INTERVALS": 13,
		"HIVE_VOTE_REGENERATION_SECONDS": 432000,
		"HIVE_MAX_SIG_CHECK_DEPTH":       2,
		"HIVE_MAX_SIG_CHECK_ACCOUNTS":    125,
	}
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) { return cfg, nil }
}

// dgpoResult builds get_dynamic_global_properties. legacy=true → "1.234 HIVE"
// string assets (condenser); false → HF26 NAI objects (database_api).
func dgpoResult(d objects.DynamicGlobalProperty, legacy bool) map[string]interface{} {
	asJSON := func(a asset.Asset) interface{} {
		if legacy {
			return legacyAsset(a)
		}
		return a
	}
	return map[string]interface{}{
		"head_block_number":               d.HeadBlockNumber,
		"head_block_id":                   hexID(d.HeadBlockID),
		"time":                            timeString(d.Time),
		"current_witness":                 d.CurrentWitness,
		"total_pow":                       d.TotalPow,
		"num_pow_witnesses":               d.NumPowWitnesses,
		"virtual_supply":                  asJSON(hiveAsset(d.VirtualSupply)),
		"current_supply":                  asJSON(hiveAsset(d.CurrentSupply)),
		"current_hbd_supply":              asJSON(hbdAsset(d.CurrentHBDSupply)),
		"total_vesting_fund_hive":         asJSON(hiveAsset(d.TotalVestingFundHive)),
		"total_vesting_shares":            asJSON(vestsAsset(d.TotalVestingShares)),
		"total_reward_fund_hive":          asJSON(hiveAsset(d.TotalRewardFundHive)),
		"total_reward_shares2":            u128dec(d.TotalRewardShares2),
		"pending_rewarded_vesting_shares": asJSON(vestsAsset(d.PendingRewardedVestingShares)),
		"pending_rewarded_vesting_hive":   asJSON(hiveAsset(d.PendingRewardedVestingHive)),
		"hbd_interest_rate":               d.HBDInterestRate,
		"hbd_print_rate":                  d.HBDPrintRate,
		"hbd_start_percent":               d.HBDStartPercent,
		"hbd_stop_percent":                d.HBDStopPercent,
		"content_reward_percent":          d.ContentRewardPercent,
		"vesting_reward_percent":          d.VestingRewardPercent,
		"proposal_fund_percent":           d.ProposalFundPercent,
		"downvote_pool_percent":           d.DownvotePoolPercent,
		"reverse_auction_seconds":         d.ReverseAuctionSeconds,
		"delegation_return_period":        d.DelegationReturnPeriod,
		"available_account_subsidies":     d.AvailableAccountSubsidies,
		"current_aslot":                   d.CurrentAslot,
		"last_irreversible_block_num":     d.LastIrreversibleBlockNum,
		"next_maintenance_time":           timeString(d.NextMaintenanceTime),
		"last_budget_time":                timeString(d.LastBudgetTime),
		"next_daily_maintenance_time":     timeString(d.NextDailyMaintenanceTime),
		"dhf_interval_ledger":             asJSON(hbdAsset(d.DHFIntervalLedger)),
	}
}

// priceResult renders an asset.Price ({base, quote}).
func priceResult(p asset.Price, legacy bool) map[string]interface{} {
	if legacy {
		return map[string]interface{}{"base": legacyAsset(p.Base), "quote": legacyAsset(p.Quote)}
	}
	return map[string]interface{}{"base": p.Base, "quote": p.Quote}
}

func feedHistoryResult(fh objects.FeedHistory, legacy bool) map[string]interface{} {
	hist := make([]map[string]interface{}, 0, len(fh.PriceHistory))
	for _, p := range fh.PriceHistory {
		hist = append(hist, priceResult(p, legacy))
	}
	return map[string]interface{}{
		"id":                     0,
		"current_median_history": priceResult(fh.CurrentMedianHistory, legacy),
		"market_median_history":  priceResult(fh.MarketMedianHistory, legacy),
		"current_min_history":    priceResult(fh.CurrentMinHistory, legacy),
		"current_max_history":    priceResult(fh.CurrentMaxHistory, legacy),
		"price_history":          hist,
	}
}

// curveName maps a CurveID to hived's string form.
func curveName(c objects.CurveID) string {
	switch c {
	case 0:
		return "quadratic"
	case 1:
		return "bounded_curation"
	case 2:
		return "linear"
	case 3:
		return "square_root"
	case 4:
		return "convergent_linear"
	case 5:
		return "convergent_square_root"
	}
	return "none"
}

func rewardFundResult(rf objects.RewardFund, legacy bool) map[string]interface{} {
	bal := func(a int64) interface{} {
		if legacy {
			return legacyAsset(hiveAsset(a))
		}
		return hiveAsset(a)
	}
	return map[string]interface{}{
		"id":                       0,
		"name":                     rf.Name,
		"reward_balance":           bal(rf.RewardBalance),
		"recent_claims":            u128dec(rf.RecentClaims),
		"last_update":              timeString(rf.LastUpdate),
		"content_constant":         itoa64(rf.ContentConstant),
		"percent_curation_rewards": rf.PercentCurationRewards,
		"percent_content_rewards":  rf.PercentContentRewards,
		"author_reward_curve":      curveName(rf.AuthorRewardCurve),
		"curation_reward_curve":    curveName(rf.CurationRewardCurve),
	}
}

func (s *Server) mGetDGPO(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		var out map[string]interface{}
		if err := s.chain.WithReadSession(func(sess *state.Session) error {
			d, e := objects.GetDGPO(sess)
			if e != nil {
				return e
			}
			out = dgpoResult(d, legacy)
			return nil
		}); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "get_dynamic_global_properties: %v", err)
		}
		return out, nil
	}
}

func (s *Server) mGetVersion() jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return map[string]interface{}{
			"blockchain_version": "1.27.0",
			"hive_revision":      "gopherd",
			"fc_revision":        "gopherd",
			"chain_id":           networkChainIDHex(),
			"node_type":          "gopherd",
		}, nil
	}
}

func (s *Server) mGetFeedHistory(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		var out map[string]interface{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			fh, _ := objects.GetFeedHistory(sess) // zero value if absent (pre-first-feed)
			out = feedHistoryResult(fh, legacy)
			return nil
		})
		return out, nil
	}
}

func (s *Server) mGetCurrentPriceFeed(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		var out map[string]interface{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			fh, _ := objects.GetFeedHistory(sess)
			out = priceResult(fh.CurrentMedianHistory, legacy)
			return nil
		})
		return out, nil
	}
}

func (s *Server) mGetActiveWitnesses() jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		var names []string
		if err := s.chain.WithReadSession(func(sess *state.Session) error {
			ws, e := objects.GetWitnessSchedule(sess)
			if e != nil {
				return e
			}
			for _, n := range ws.CurrentShuffledWitnesses {
				names = append(names, string(n))
			}
			return nil
		}); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "get_active_witnesses: %v", err)
		}
		return map[string]interface{}{"witnesses": names}, nil
	}
}

func (s *Server) mGetWitnessSchedule() jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		var out map[string]interface{}
		if err := s.chain.WithReadSession(func(sess *state.Session) error {
			ws, e := objects.GetWitnessSchedule(sess)
			if e != nil {
				return e
			}
			names := make([]string, 0, len(ws.CurrentShuffledWitnesses))
			for _, n := range ws.CurrentShuffledWitnesses {
				names = append(names, string(n))
			}
			out = map[string]interface{}{
				"id":                          0,
				"current_virtual_time":        u128dec(ws.CurrentVirtualTime),
				"next_shuffle_block_num":      ws.NextShuffleBlockNum,
				"current_shuffled_witnesses":  names,
				"num_scheduled_witnesses":     ws.NumScheduledWitnesses,
				"elected_weight":              ws.ElectedWeight,
				"timeshare_weight":            ws.TimeshareWeight,
				"miner_weight":                ws.MinerWeight,
				"witness_pay_normalization_factor": ws.WitnessPayNormalizationFactor,
				"majority_version":            versionString(ws.MajorityVersion),
				"max_voted_witnesses":         ws.MaxVotedWitnesses,
				"max_miner_witnesses":         ws.MaxMinerWitnesses,
				"max_runner_witnesses":        ws.MaxRunnerWitnesses,
				"hardfork_required_witnesses": ws.HardforkRequiredWitnesses,
			}
			return nil
		}); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "get_witness_schedule: %v", err)
		}
		return out, nil
	}
}

func (s *Server) mGetRewardFunds(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		funds := []map[string]interface{}{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			rf, e := objects.GetRewardFund(sess, "post")
			if e == nil {
				funds = append(funds, rewardFundResult(rf, legacy))
			}
			return nil
		})
		return map[string]interface{}{"funds": funds}, nil
	}
}

func (s *Server) mGetHardforkProps() jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		var out map[string]interface{}
		if err := s.chain.WithReadSession(func(sess *state.Session) error {
			hp, e := objects.GetHardforkPropertyObject(sess)
			if e != nil {
				return e
			}
			out = map[string]interface{}{
				"id":                       0,
				"last_hardfork":            hp.LastHardfork,
				"current_hardfork_version": "1." + itoa(hp.LastHardfork) + ".0",
			}
			return nil
		}); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "get_hardfork_properties: %v", err)
		}
		return out, nil
	}
}
