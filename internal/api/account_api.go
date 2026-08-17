package api

import (
	"context"
	"encoding/json"
	"strconv"

	"gopherd/internal/api/jsonrpc"
	"gopherd/internal/crypto"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// authorityResult renders an auth.Authority as hived's
// {weight_threshold, account_auths:[[name,w]], key_auths:[["STM..",w]]}.
func authorityResult(a auth.Authority) map[string]interface{} {
	accts := make([][]interface{}, 0, len(a.AccountAuths))
	for _, aa := range a.AccountAuths {
		accts = append(accts, []interface{}{string(aa.Account), uint16(aa.Weight)})
	}
	keys := make([][]interface{}, 0, len(a.KeyAuths))
	for _, ka := range a.KeyAuths {
		keys = append(keys, []interface{}{ka.Key.String(), uint16(ka.Weight)})
	}
	return map[string]interface{}{
		"weight_threshold": a.WeightThreshold,
		"account_auths":    accts,
		"key_auths":        keys,
	}
}

func manabar(current int64, lastUpdate uint32) map[string]interface{} {
	return map[string]interface{}{
		"current_mana":     strconv.FormatInt(current, 10),
		"last_update_time": lastUpdate,
	}
}

// accountResult renders an account (+ its authorities) in hived's shape. legacy
// controls asset form.
func accountResult(a objects.Account, au objects.AccountAuthority, legacy bool) map[string]interface{} {
	hv := func(x int64) interface{} {
		if legacy {
			return legacyAsset(hiveAsset(x))
		}
		return hiveAsset(x)
	}
	sbd := func(x int64) interface{} {
		if legacy {
			return legacyAsset(hbdAsset(x))
		}
		return hbdAsset(x)
	}
	vs := func(x int64) interface{} {
		if legacy {
			return legacyAsset(vestsAsset(x))
		}
		return vestsAsset(x)
	}
	pvsf := make([]string, len(a.ProxiedVSFVotes))
	for i, v := range a.ProxiedVSFVotes {
		pvsf[i] = strconv.FormatInt(v, 10)
	}
	memo := crypto.PublicKey(a.MemoKey).String()
	proxy := string(a.Proxy)
	return map[string]interface{}{
		"name":                          string(a.Name),
		"owner":                         authorityResult(au.Owner),
		"active":                        authorityResult(au.Active),
		"posting":                       authorityResult(au.Posting),
		"memo_key":                      memo,
		"json_metadata":                 a.JSONMetadata,
		"proxy":                         proxy,
		"last_owner_update":             timeString(a.LastOwnerUpdate),
		"created":                       timeString(a.Created),
		"recovery_account":              string(a.RecoveryAccount),
		"post_count":                    a.PostCount,
		"can_vote":                      !a.VoteRevoked,
		"voting_manabar":                manabar(a.VotingManaCurrent, a.VotingManaLastUpdate),
		"downvote_manabar":              manabar(a.DownvoteManaCurrent, a.DownvoteManaLastUpdate),
		"balance":                       hv(a.Balance),
		"savings_balance":               hv(a.SavingsBalance),
		"hbd_balance":                   sbd(a.HBDBalance),
		"savings_hbd_balance":           sbd(a.SavingsHBDBalance),
		"reward_hive_balance":           hv(a.RewardHive),
		"reward_hbd_balance":            sbd(a.RewardHBD),
		"reward_vesting_balance":        vs(a.RewardVests),
		"reward_vesting_hive":           hv(a.RewardVestingHive),
		"vesting_shares":                vs(a.VestingShares),
		"delegated_vesting_shares":      vs(a.DelegatedVestingShares),
		"received_vesting_shares":       vs(a.ReceivedVestingShares),
		"vesting_withdraw_rate":         vs(a.VestingWithdrawRate),
		"next_vesting_withdrawal":       timeString(a.NextVestingWithdrawal),
		"withdrawn":                     a.Withdrawn,
		"to_withdraw":                   a.ToWithdraw,
		"witnesses_voted_for":           a.WitnessesVotedFor,
		"proxied_vsf_votes":             pvsf,
		"last_vote_time":                timeString(a.LastVoteTime),
		"last_post":                     timeString(a.LastPost),
		"last_root_post":                timeString(a.LastRootPost),
		"governance_vote_expiration_ts": timeString(a.GovernanceVoteExpiration),
		"pending_claimed_accounts":      a.PendingClaimedAccounts,
	}
}

// loadAccounts fetches accounts (+ authorities) by name, skipping unknown ones.
func (s *Server) loadAccounts(names []string, legacy bool) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(names))
	_ = s.chain.WithReadSession(func(sess *state.Session) error {
		for _, n := range names {
			acct, err := objects.GetAccount(sess, auth.AccountName(n))
			if err != nil {
				continue
			}
			au, _ := objects.GetAccountAuthority(sess, auth.AccountName(n))
			out = append(out, accountResult(acct, au, legacy))
		}
		return nil
	})
	return out
}

// namesFromParams decodes account-name lists from both conventions:
//
//	condenser get_accounts: [["alice","bob"]]
//	database_api find_accounts: {"accounts":["alice","bob"]}
func namesFromParams(params json.RawMessage) ([]string, error) {
	var obj struct {
		Accounts []string `json:"accounts"`
	}
	if err := json.Unmarshal(params, &obj); err == nil && obj.Accounts != nil {
		return obj.Accounts, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(params, &arr); err == nil && len(arr) > 0 {
		var names []string
		if err := json.Unmarshal(arr[0], &names); err == nil {
			return names, nil
		}
	}
	return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "expected {\"accounts\":[...]} or [[...]]")
}

// mFindAccounts: database_api.find_accounts {"accounts":[...]} → {"accounts":[...]}.
func (s *Server) mFindAccounts(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		names, err := namesFromParams(params)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"accounts": s.loadAccounts(names, legacy)}, nil
	}
}

// mGetAccounts: condenser_api.get_accounts [["a","b"]] → [account,...] (bare array).
func (s *Server) mGetAccounts(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		names, err := namesFromParams(params)
		if err != nil {
			return nil, err
		}
		return s.loadAccounts(names, legacy), nil
	}
}

// mGetAccountCount: condenser_api.get_account_count [] → total account count.
func (s *Server) mGetAccountCount() jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		n := 0
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			// O(1) store count — not an O(N) scan under the apply read lock.
			n = objects.EnsureAccountStore(sess).Count()
			return nil
		})
		return n, nil
	}
}

// mLookupAccounts: condenser_api.lookup_accounts ["lower", limit] → up to `limit`
// account names in ascending order starting at lower_bound.
func (s *Server) mLookupAccounts() jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		var arr []json.RawMessage
		if json.Unmarshal(params, &arr) != nil || len(arr) < 2 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "expected [lower_bound, limit]")
		}
		var lower string
		var limit int
		_ = json.Unmarshal(arr[0], &lower)
		_ = json.Unmarshal(arr[1], &limit)
		if limit <= 0 || limit > 1000 {
			limit = 1000
		}
		// RangeAccounts yields ascending name order: skip below the lower bound,
		// then stop once `limit` names are collected — bounds the work to `limit`
		// (the scan runs under the apply read lock, so an unbounded collect would
		// stall ApplyBlock).
		names := make([]string, 0, limit)
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			return objects.RangeAccounts(sess, func(a *objects.Account) (bool, error) {
				if string(a.Name) < lower {
					return true, nil
				}
				names = append(names, string(a.Name))
				return len(names) < limit, nil
			})
		})
		return names, nil
	}
}
