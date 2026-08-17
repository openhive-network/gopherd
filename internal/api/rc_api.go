package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"

	"gopherd/internal/api/jsonrpc"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// rcResourceNames mirrors fc::reflector<rc_resource_types>::to_string — the
// five RC resource names, in RCResource enum order.
var rcResourceNames = [objects.RCNumResources]string{
	"resource_history_bytes",
	"resource_new_accounts",
	"resource_market_bytes",
	"resource_state_bytes",
	"resource_execution_time",
}

// registerRCAPI wires the rc_api surface (hived's rc_api_plugin) plus the three
// condenser_api aliases. Every method is a read over committed RC state:
// per-account RC fields (find/list_rc_accounts), the rc_direct_delegation table
// (list_rc_direct_delegations), the singleton rc_pool / rc_resource_param
// objects (get_resource_pool / get_resource_params) and the archived
// rc_stats_object (get_rc_stats / get_rc_operation_stats).
func (s *Server) registerRCAPI() {
	s.reg.Register("rc_api.get_resource_params", s.mGetResourceParams())
	s.reg.Register("rc_api.get_resource_pool", s.mGetResourcePool())
	s.reg.Register("rc_api.find_rc_accounts", s.mFindRCAccounts(false, false))
	s.reg.Register("rc_api.list_rc_accounts", s.mListRCAccounts(false, false))
	s.reg.Register("rc_api.list_rc_direct_delegations", s.mListRCDirectDelegations(false, false))
	s.reg.Register("rc_api.get_rc_stats", s.mGetRCStats())
	s.reg.Register("rc_api.get_rc_operation_stats", s.mGetRCOperationStats())

	// condenser_api variants: legacy asset form + bare-array returns.
	s.reg.Register("condenser_api.find_rc_accounts", s.mFindRCAccounts(true, true))
	s.reg.Register("condenser_api.list_rc_accounts", s.mListRCAccounts(true, true))
	s.reg.Register("condenser_api.list_rc_direct_delegations", s.mListRCDirectDelegations(true, true))
}

// --- rc_account rendering ----------------------------------------------------

// rcAccountResult mirrors rc_account_api_object. rc_manabar is the STORED
// manabar (hived does not regenerate it for the API). max_rc is computed live
// (effective_vesting_excl_withdraw + delegated_in - delegated_out + adjustment);
// delegated_rc is what this account delegates OUT, received_delegated_rc what it
// receives IN. max_rc_creation_adjustment is a VESTS asset.
func rcAccountResult(a *objects.Account, legacy bool) map[string]interface{} {
	adj := vestsAsset(a.RCAdjustment)
	var adjOut interface{} = adj
	if legacy {
		adjOut = legacyAsset(adj)
	}
	return map[string]interface{}{
		"account":                    string(a.Name),
		"rc_manabar":                 manabar(a.RCManaCurrent, a.RCManaLastUpdate),
		"max_rc_creation_adjustment": adjOut,
		"max_rc":                     a.MaximumRC(),
		"delegated_rc":               uint64(a.DelegatedRCOut),
		"received_delegated_rc":      uint64(a.DelegatedRCIn),
	}
}

// --- find_rc_accounts / list_rc_accounts -------------------------------------

// accountsParam extracts the list of account names from either the database_api
// named form {"accounts":[...]} or the condenser positional form [[...]].
func accountsParam(params json.RawMessage) []string {
	var named struct {
		Accounts []string `json:"accounts"`
	}
	if json.Unmarshal(params, &named) == nil && named.Accounts != nil {
		return named.Accounts
	}
	var arr []json.RawMessage
	if json.Unmarshal(params, &arr) == nil && len(arr) > 0 {
		var names []string
		if json.Unmarshal(arr[0], &names) == nil {
			return names
		}
	}
	return nil
}

func (s *Server) mFindRCAccounts(condenserArgs, legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		names := accountsParam(params)
		if len(names) > 1000 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "too many accounts (max 1000)")
		}
		out := make([]map[string]interface{}, 0, len(names))
		err := s.chain.WithReadSession(func(sess *state.Session) error {
			for _, n := range names {
				a, e := objects.GetAccount(sess, auth.AccountName(n))
				if e != nil {
					continue // missing account → skipped, matching hived
				}
				out = append(out, rcAccountResult(&a, legacy))
			}
			return nil
		})
		if err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "find_rc_accounts: %v", err)
		}
		if condenserArgs {
			return out, nil
		}
		return map[string]interface{}{"rc_accounts": out}, nil
	}
}

// startLimitParam pulls (start, limit) from the database_api named form
// {"start":..,"limit":N} or the condenser positional [start, limit].
func startLimitParam(params json.RawMessage) (json.RawMessage, int) {
	var named struct {
		Start json.RawMessage `json:"start"`
		Limit int             `json:"limit"`
	}
	if json.Unmarshal(params, &named) == nil && (named.Start != nil || named.Limit != 0) {
		return named.Start, named.Limit
	}
	var arr []json.RawMessage
	if json.Unmarshal(params, &arr) == nil && len(arr) > 0 {
		limit := 0
		if len(arr) >= 2 {
			_ = json.Unmarshal(arr[1], &limit)
		}
		return arr[0], limit
	}
	return nil, 0
}

func (s *Server) mListRCAccounts(condenserArgs, legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		startRaw, limit := startLimitParam(params)
		if limit <= 0 || limit > 1000 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "limit not set or too big")
		}
		var start string
		_ = json.Unmarshal(startRaw, &start)

		// RangeAccounts (RangePrimary) yields accounts in ascending name order,
		// so we skip names < start and stop as soon as we've collected `limit`
		// — matching hived's by_name lower_bound walk. Building a result only
		// for the returned page (not for every account ≥ start) bounds the work
		// to `limit`, which matters because the whole scan runs under the apply
		// read lock: an unbounded scan would stall ApplyBlock.
		out := make([]map[string]interface{}, 0, limit)
		err := s.chain.WithReadSession(func(sess *state.Session) error {
			return objects.RangeAccounts(sess, func(a *objects.Account) (bool, error) {
				if string(a.Name) < start {
					return true, nil // keep skipping toward the lower bound
				}
				out = append(out, rcAccountResult(a, legacy))
				return len(out) < limit, nil // stop once the page is full
			})
		})
		if err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "list_rc_accounts: %v", err)
		}
		if condenserArgs {
			return out, nil
		}
		return map[string]interface{}{"rc_accounts": out}, nil
	}
}

// --- list_rc_direct_delegations ----------------------------------------------

// mListRCDirectDelegations lists direct RC delegations for one delegator,
// starting at (from, to). NOTE: hived paginates by account_id order (creation
// order) of the delegatee; gopherd's rc_direct_delegation table is keyed by
// (from_name, to_name), so we paginate by delegatee NAME. The full set of a
// delegator's delegations is identical; only intra-delegator ordering differs.
func (s *Server) mListRCDirectDelegations(condenserArgs, legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		startRaw, limit := startLimitParam(params)
		if limit <= 0 || limit > 1000 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "limit not set or too big")
		}
		var pair []string
		if json.Unmarshal(startRaw, &pair) != nil || len(pair) != 2 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "by_from start requires 2 values (from, to)")
		}
		from, toStart := pair[0], pair[1]

		type dele struct {
			to string
			rc uint64
		}
		var found []dele
		err := s.chain.WithReadSession(func(sess *state.Session) error {
			rangeTable(sess, objects.RCDirectDelegationTable, func(value []byte) bool {
				d, e := objects.ReadRCDirectDelegation(value)
				if e != nil {
					return true
				}
				if string(d.From) == from && string(d.To) >= toStart {
					found = append(found, dele{string(d.To), d.DelegatedRC})
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "list_rc_direct_delegations: %v", err)
		}
		sort.Slice(found, func(i, j int) bool { return found[i].to < found[j].to })
		if len(found) > limit {
			found = found[:limit]
		}
		out := make([]map[string]interface{}, len(found))
		for i, d := range found {
			out[i] = map[string]interface{}{
				"from":         from,
				"to":           d.to,
				"delegated_rc": d.rc,
			}
		}
		if condenserArgs {
			return out, nil
		}
		return map[string]interface{}{"rc_direct_delegations": out}, nil
	}
}

// --- get_resource_pool -------------------------------------------------------

// mGetResourcePool mirrors get_resource_pool: per-resource {pool, fill_level},
// where fill_level = pool * HIVE_100_PERCENT / pool_eq (bps of equilibrium).
func (s *Server) mGetResourcePool() jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		pools := map[string]interface{}{}
		err := s.chain.WithReadSession(func(sess *state.Session) error {
			pool, e := objects.GetRCPool(sess)
			if e != nil {
				return e
			}
			params, e := objects.GetRCResourceParam(sess)
			if e != nil {
				return e
			}
			for i := 0; i < objects.RCNumResources; i++ {
				p := pool.PoolArray[i]
				poolEq := int64(params.Entries[i].Dynamics.PoolEq)
				var fill int64
				if poolEq != 0 {
					// p*10000 can exceed int64 for large pools (state/exec
					// resources reach ~1e15+), so do it in big.Int like hived's
					// uint128 path; the quotient is small (≈ bps of equilibrium).
					q := new(big.Int).Mul(big.NewInt(p), big.NewInt(10000))
					q.Quo(q, big.NewInt(poolEq))
					fill = q.Int64()
				}
				pools[rcResourceNames[i]] = map[string]interface{}{
					"pool":       p,
					"fill_level": uint16(fill),
				}
			}
			return nil
		})
		if err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "get_resource_pool: %v", err)
		}
		return map[string]interface{}{"resource_pool": pools}, nil
	}
}

// --- get_resource_params -----------------------------------------------------

// mGetResourceParams mirrors get_resource_params: resource_names, the
// per-resource dynamics + price-curve params from the rc_resource_param
// singleton, and the static size_info (state-byte + exec-time cost tables).
func (s *Server) mGetResourceParams() jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		resourceParams := map[string]interface{}{}
		err := s.chain.WithReadSession(func(sess *state.Session) error {
			params, e := objects.GetRCResourceParam(sess)
			if e != nil {
				return e
			}
			for i := 0; i < objects.RCNumResources; i++ {
				e := params.Entries[i]
				resourceParams[rcResourceNames[i]] = map[string]interface{}{
					"resource_dynamics_params": map[string]interface{}{
						"resource_unit":        e.Dynamics.ResourceUnit,
						"budget_per_time_unit": e.Dynamics.BudgetPerTime,
						"pool_eq":              e.Dynamics.PoolEq,
						"max_pool_size":        e.Dynamics.MaxPool,
						"decay_params": map[string]interface{}{
							"decay_per_time_unit":             e.Dynamics.DecayPerTime,
							"decay_per_time_unit_denom_shift": e.Dynamics.DecayDenomShift,
						},
						"min_decay": e.Dynamics.MinDecay,
					},
					"price_curve_params": map[string]interface{}{
						"coeff_a": e.Curve.CoeffA,
						"coeff_b": e.Curve.CoeffB,
						"shift":   e.Curve.Shift,
					},
				}
			}
			return nil
		})
		if err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "get_resource_params: %v", err)
		}
		names := make([]string, objects.RCNumResources)
		copy(names, rcResourceNames[:])
		return map[string]interface{}{
			"resource_names":  names,
			"resource_params": resourceParams,
			"size_info": map[string]interface{}{
				"resource_state_bytes":    stateObjectSizeInfo(),
				"resource_execution_time": operationExecInfo(),
			},
		}, nil
	}
}

// stateObjectSizeInfo reproduces state_object_size_info (resource_sizes.cpp,
// USE_FIXED_SIZE build). Values are hour-bytes: object size × lifetime. The
// arithmetic is kept in C++ form so it can be diffed against the reference.
func stateObjectSizeInfo() map[string]interface{} {
	const (
		temporary  = int64(1)
		lasting    = temporary * 5 * 365 * 12 // 2.5 years
		persistent = temporary * 5 * 365 * 24 // 5 years
	)
	vestingDelegationObject := 88 * lasting
	vestingDelegationExpiration := 120 * temporary * 5 * 24
	delegateVestingShares := vestingDelegationObject
	if vestingDelegationExpiration > delegateVestingShares {
		delegateVestingShares = vestingDelegationExpiration
	}
	return map[string]interface{}{
		"TEMPORARY_STATE_BYTE":  temporary,
		"LASTING_STATE_BYTE":    lasting,
		"PERSISTENT_STATE_BYTE": persistent,

		"account_create_base_size":      (616+144)*persistent + 312*persistent,
		"authority_account_member_size": 24 * persistent,
		"authority_key_member_size":     36 * persistent,

		"owner_authority_history_object_size": 168 * temporary * 30 * 24,
		"transfer_to_vesting_size":            16 * temporary * 30 * 24,
		"request_account_recovery_size":       192 * temporary * 24,
		"change_recovery_account_size":        136 * temporary * 30 * 24,

		"comment_base_size":                 96*persistent + 192*temporary*7*24,
		"comment_permlink_char_size":        temporary * 7 * 24,
		"comment_beneficiaries_member_size": 8 * temporary * 7 * 24,

		"vote_size": 144 * temporary * 7 * 24,

		"convert_size":                120 * temporary * 7 * 12,
		"collateralized_convert_size": 128 * temporary * 7 * 12,

		"decline_voting_rights_size": 128 * temporary * 30 * 24,
		"escrow_transfer_size":       216 * lasting,
		"limit_order_create_size":    208 * temporary * 28 * 24,
		"transfer_from_savings_size": 232 * temporary * 3 * 24,
		"transaction_base_size":      128 * temporary * 1,

		"vesting_delegation_object_size":            vestingDelegationObject,
		"vesting_delegation_expiration_object_size": vestingDelegationExpiration,
		"delegate_vesting_shares_size":              delegateVestingShares,

		"set_withdraw_vesting_route_size": 144 * lasting,

		"witness_update_base_size":     544 * persistent,
		"witness_update_url_char_size": persistent,

		"account_witness_vote_size": 136 * temporary * 365 * 24,

		"create_proposal_base_size":                  336 * temporary,
		"create_proposal_subject_permlink_char_size": temporary,
		"update_proposal_votes_member_size":          128 * temporary * 365 * 24,

		"recurrent_transfer_base_size":      200 * temporary,
		"recurrent_transfer_memo_char_size": temporary,

		"delegate_rc_base_size": 88 * lasting,
	}
}

// operationExecInfo reproduces operation_exec_info (resource_sizes.cpp) — the
// per-op execution-time cost table (nanoseconds), kept in C++ sum form.
func operationExecInfo() map[string]interface{} {
	const vestingWithdrawIntervals = 13 // HIVE_VESTING_WITHDRAW_INTERVALS
	return map[string]interface{}{
		"account_create_time":                 498 + 31575 + 236 + 6401,
		"account_create_with_delegation_time": 770 + 39616 + 139 + 66,
		"account_witness_vote_time":           516 + 13907 + 173 + 145,
		"comment_time":                        2419 + 20408 + 221 + 166 + 42964,
		"comment_options_time":                413 + 5530 + 142 + 117,
		"convert_time":                        442 + 11491 + 207 + 143 + 8280,
		"collateralized_convert_time":         613 + 13325 + 332 + 239 + 9154,
		"create_claimed_account_time":         964 + 46331 + 342 + 10778,
		"decline_voting_rights_time":          530 + 3894 + 179 + 187 + 24301,
		"delegate_vesting_shares_time":        411 + 11998 + 3110 + 1208,
		"escrow_transfer_time":                2943 + 16130 + 309 + 204,
		"limit_order_create_time":             468 + 13924 + 187 + 124,
		"limit_order_create2_time":            607 + 23356 + 310 + 118,
		"request_account_recovery_time":       613 + 7058 + 149 + 127,
		"set_withdraw_vesting_route_time":     501 + 9003 + 632 + 121,
		"vote_time":                           479 + 17568 + 187 + 78,
		"witness_update_time":                 675 + 3287 + 154 + 121,
		"transfer_time":                       638 + 5023 + 204 + 134,
		"transfer_to_vesting_time":            484 + 15856 + 2174 + 721 + 23010,
		"transfer_to_savings_time":            771 + 5392 + 205 + 155,
		"transfer_from_savings_time":          519 + 7840 + 111 + 106 + 8974,
		"claim_reward_balance_time":           427 + 23366 + 2798 + 970,
		"withdraw_vesting_time":               376 + 6524 + 2068 + 567 + 16307*vestingWithdrawIntervals,
		"account_update_time":                 1531 + 11433 + 206 + 152,
		"account_update2_time":                2182 + 10804 + 405 + 257,
		"account_witness_proxy_time":          684 + 57106 + 170 + 166,
		"cancel_transfer_from_savings_time":   135 + 2881 + 72 + 66,
		"change_recovery_account_time":        580 + 9391 + 297 + 202,
		"claim_account_time":                  336 + 8028 + 267 + 150,
		"custom_time":                         132 + 183 + 238 + 121,
		"custom_json_time":                    859 + 162 + 321 + 167,
		"custom_binary_time":                  0 + 0 + 0 + 0 + 100000,
		"delete_comment_time":                 521 + 17208 + 175 + 146,
		"escrow_approve_time":                 814 + 8408 + 260 + 181,
		"escrow_dispute_time":                 918 + 5219 + 261 + 176,
		"escrow_release_time":                 1313 + 13586 + 252 + 189,
		"feed_publish_time":                   469 + 2233 + 213 + 144,
		"limit_order_cancel_time":             407 + 5986 + 204 + 142,
		"witness_set_properties_time":         3986 + 4920 + 310 + 234,
		"create_proposal_time":                831 + 24323 + 5666 + 2168,
		"update_proposal_time":                820 + 11193 + 4501 + 267,
		"update_proposal_votes_time":          503 + 12602 + 2993 + 1206,
		"remove_proposal_time":                527 + 129323 + 3886 + 1692,
		"recurrent_transfer_base_time":        914 + 12426 + 330 + 238,
		"recurrent_transfer_processing_time":  14347,
		"delegate_rc_time":                    75000,
		"verify_authority_time":               94165,
		"transaction_time":                    2821 + 386 + 3415,
		"recover_account_time":                727 + 17934 + 149 + 145,
		"pow_time":                            177321 + 23458 + 165 + 57,
		"pow2_time":                           86349 + 17226 + 160 + 92,
	}
}

// --- get_rc_stats / get_rc_operation_stats -----------------------------------

// mGetRCStats returns the REGULAR report over the archived rc_stats_object,
// mirroring resource_credits::get_report(REGULAR, archive).
func (s *Server) mGetRCStats() jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		var report map[string]interface{}
		err := s.chain.WithReadSession(func(sess *state.Session) error {
			if !objects.HasRCStats(sess, objects.RCStatsIDArchive) {
				return fmt.Errorf("Gathering of RC stats not enabled yet")
			}
			st, e := objects.GetRCStats(sess, objects.RCStatsIDArchive)
			if e != nil {
				return e
			}
			report = rcStatsReport(&st)
			return nil
		})
		if err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "get_rc_stats: %v", err)
		}
		return map[string]interface{}{"rc_stats": report}, nil
	}
}

// rcStatsReport builds the REGULAR-verbosity report (ops with avg_cost, payers
// without cost/usage arrays).
func rcStatsReport(st *objects.RCStats) map[string]interface{} {
	budget := make([]int64, objects.RCNumResources)
	pool := make([]int64, objects.RCNumResources)
	share := make([]uint16, objects.RCNumResources)
	for i := 0; i < objects.RCNumResources; i++ {
		budget[i] = st.Budget[i]
		pool[i] = st.Pool[i]
		share[i] = st.Share[i]
	}

	ops := map[string]interface{}{}
	for i := 0; i <= objects.RCNumOperations; i++ {
		os := &st.OpStats[i]
		if os.Count == 0 {
			continue
		}
		name := "multiop"
		if i < objects.RCNumOperations {
			name = op.Tag(i).Name()
		}
		ops[name] = map[string]interface{}{
			"count":    os.Count,
			"avg_cost": opStatsAverageCost(os),
		}
	}

	payers := []interface{}{}
	for i := 0; i < objects.RCNumPayerRanks; i++ {
		ps := &st.PayerStats[i]
		if ps.Count == 0 {
			continue
		}
		p := map[string]interface{}{
			"rank":  i,
			"count": ps.Count,
		}
		if ps.LessThan5Percent > 0 {
			p["lt5"] = ps.LessThan5Percent
		}
		if ps.LessThan10Percent > 0 {
			p["lt10"] = ps.LessThan10Percent
		}
		if ps.LessThan20Percent > 0 {
			p["lt20"] = ps.LessThan20Percent
		}
		if ps.CantAfford[0] != 0 || ps.CantAfford[1] != 0 || ps.CantAfford[2] != 0 {
			p["cant_afford"] = map[string]interface{}{
				"vote":     ps.CantAfford[0],
				"comment":  ps.CantAfford[1],
				"transfer": ps.CantAfford[2],
			}
		}
		payers = append(payers, p)
	}

	return map[string]interface{}{
		"block":    st.BlockNum,
		"stamp":    hex.EncodeToString(st.Stamp[:]),
		"regen":    st.Regen,
		"budget":   budget,
		"pool":     pool,
		"share":    share,
		"vote":     st.AverageCost[0],
		"comment":  st.AverageCost[1],
		"transfer": st.AverageCost[2],
		"ops":      ops,
		"payers":   payers,
	}
}

// opStatsAverageCost mirrors rc_op_stats::average_cost — sum of per-resource
// cost divided by observation count.
func opStatsAverageCost(os *objects.RCOpStats) int64 {
	if os.Count == 0 {
		return 0
	}
	var sum int64
	for i := 0; i < objects.RCNumResources; i++ {
		sum += os.Cost[i]
	}
	return sum / int64(os.Count)
}

// mGetRCOperationStats returns averaged cost/share/usage for one operation,
// mirroring get_rc_operation_stats over the archived rc_stats_object.
func (s *Server) mGetRCOperationStats() jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		opName := rcOperationParam(params)
		opID, ok := rcOpID(opName)
		if !ok {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "unknown operation %q", opName)
		}
		var result map[string]interface{}
		err := s.chain.WithReadSession(func(sess *state.Session) error {
			if !objects.HasRCStats(sess, objects.RCStatsIDArchive) {
				return fmt.Errorf("Gathering of RC stats not enabled yet")
			}
			st, e := objects.GetRCStats(sess, objects.RCStatsIDArchive)
			if e != nil {
				return e
			}
			os := &st.OpStats[opID]
			avg := opStatsAverageCost(os)
			var cost, usage [objects.RCNumResources]int64
			var share [objects.RCNumResources]uint16
			if avg > 0 {
				for i := 0; i < objects.RCNumResources; i++ {
					cost[i] = os.Cost[i] / int64(os.Count)
					usage[i] = os.Usage[i] / int64(os.Count)
					share[i] = uint16(cost[i] * 10000 / avg)
				}
			}
			// resource_unit denominators [1,10000,10,1,1] per hived's note.
			tokens := strconv.FormatFloat(float64(usage[objects.RCNewAccounts])/10000.0, 'f', 4, 64)
			result = map[string]interface{}{
				"count":       os.Count,
				"avg_cost_rc": avg,
				"resource_cost": map[string]interface{}{
					"history_rc": cost[objects.RCHistoryBytes],
					"tokens_rc":  cost[objects.RCNewAccounts],
					"market_rc":  cost[objects.RCMarketBytes],
					"state_rc":   cost[objects.RCStateBytes],
					"exec_rc":    cost[objects.RCExecutionTime],
				},
				"resource_cost_share": map[string]interface{}{
					"history_bp": share[objects.RCHistoryBytes],
					"tokens_bp":  share[objects.RCNewAccounts],
					"market_bp":  share[objects.RCMarketBytes],
					"state_bp":   share[objects.RCStateBytes],
					"exec_bp":    share[objects.RCExecutionTime],
				},
				"resource_usage": map[string]interface{}{
					"history_bytes": usage[objects.RCHistoryBytes],
					"tokens":        tokens,
					"market_bytes":  usage[objects.RCMarketBytes] / 10,
					"state_hbytes":  usage[objects.RCStateBytes],
					"exec_ns":       usage[objects.RCExecutionTime],
				},
			}
			return nil
		})
		if err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "get_rc_operation_stats: %v", err)
		}
		return result, nil
	}
}

// rcOperationParam extracts the operation name from {"operation":"vote"} or
// ["vote"].
func rcOperationParam(params json.RawMessage) string {
	var named struct {
		Operation string `json:"operation"`
	}
	if json.Unmarshal(params, &named) == nil && named.Operation != "" {
		return named.Operation
	}
	var arr []json.RawMessage
	if json.Unmarshal(params, &arr) == nil && len(arr) > 0 {
		var s string
		if json.Unmarshal(arr[0], &s) == nil {
			return s
		}
	}
	return ""
}

// rcOpID resolves an operation name (short form, e.g. "vote") to its op tag, or
// the trailing multi-op bin for "multiop" / a bare numeric index.
func rcOpID(name string) (int, bool) {
	if name == "multiop" {
		return objects.RCNumOperations, true
	}
	for t := 0; t < objects.RCNumOperations; t++ {
		if op.Tag(t).Name() == name {
			return t, true
		}
	}
	if n, err := strconv.Atoi(name); err == nil && n >= 0 && n <= objects.RCNumOperations {
		return n, true
	}
	return 0, false
}
