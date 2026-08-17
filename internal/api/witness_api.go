package api

import (
	"context"
	"encoding/json"
	"strconv"

	"gopherd/internal/api/jsonrpc"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// registerWitnessListAPI wires the witness enumeration methods (count / lookup /
// by-vote) backed by WitnessStore.RangePrimary / RangeByVote.
func (s *Server) registerWitnessListAPI() {
	s.reg.Register("condenser_api.get_witness_count", s.mGetWitnessCount())
	s.reg.Register("condenser_api.lookup_witness_accounts", s.mLookupWitnessAccounts())
	s.reg.Register("condenser_api.get_witnesses_by_vote", s.mGetWitnessesByVote(true))
}

func (s *Server) mGetWitnessCount() jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		n := 0
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			return objects.EnsureWitnessStore(sess).RangePrimary(func(*objects.Witness) (bool, error) { n++; return true, nil })
		})
		return n, nil
	}
}

func (s *Server) mLookupWitnessAccounts() jsonrpc.Handler {
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
		// RangePrimary yields ascending owner order with early termination, so
		// skip below the lower bound and stop at `limit` — no full collect+sort.
		names := make([]string, 0, limit)
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			return objects.EnsureWitnessStore(sess).RangePrimary(func(w *objects.Witness) (bool, error) {
				if string(w.Owner) < lower {
					return true, nil
				}
				names = append(names, string(w.Owner))
				return len(names) < limit, nil
			})
		})
		return names, nil
	}
}

// mGetWitnessesByVote: ["start_name", limit] → up to `limit` witnesses ordered by
// vote descending. (start_name pagination simplified: returns the top `limit`.)
func (s *Server) mGetWitnessesByVote(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		limit := 21
		var arr []json.RawMessage
		if json.Unmarshal(params, &arr) == nil && len(arr) >= 2 {
			_ = json.Unmarshal(arr[1], &limit)
		}
		if limit <= 0 || limit > 100 {
			limit = 100
		}
		// RangeByVote iterates vote-descending with early termination, so build
		// only the top `limit` witnesses instead of copying + sorting the whole
		// witness set under the apply read lock.
		out := make([]map[string]interface{}, 0, limit)
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			return objects.EnsureWitnessStore(sess).RangeByVote(func(w *objects.Witness) (bool, error) {
				out = append(out, witnessResult(*w, legacy))
				return len(out) < limit, nil
			})
		})
		return out, nil
	}
}

func witnessResult(w objects.Witness, legacy bool) map[string]interface{} {
	fee := func(x int64) interface{} {
		if legacy {
			return legacyAsset(hiveAsset(x))
		}
		return hiveAsset(x)
	}
	return map[string]interface{}{
		"owner":                    string(w.Owner),
		"created":                  timeString(w.Created),
		"url":                      w.URL,
		"votes":                    strconv.FormatInt(w.TotalVotesVests, 10),
		"virtual_last_update":      u128dec(w.VirtualLastUpdate),
		"virtual_position":         u128dec(w.VirtualPosition),
		"virtual_scheduled_time":   u128dec(w.VirtualScheduledTime),
		"total_missed":             w.TotalMissedBlocks,
		"last_confirmed_block_num": w.LastConfirmedBlockNum,
		"pow_worker":               w.PowWorker,
		"signing_key":              w.BlockSigningKey.String(),
		"props": map[string]interface{}{
			"account_creation_fee":   fee(w.AccountCreationFee),
			"maximum_block_size":     w.MaximumBlockSize,
			"hbd_interest_rate":      w.HBDInterestRate,
			"account_subsidy_budget": w.AccountSubsidyBudget,
			"account_subsidy_decay":  w.AccountSubsidyDecay,
		},
		"hbd_exchange_rate":                   priceResult(w.HBDExchangeRate, legacy),
		"last_hbd_exchange_update":            timeString(w.LastHBDExchangeUpdate),
		"running_version":                     versionString(w.RunningVersion),
		"hardfork_version_vote":               versionString(w.HardforkVersionVote),
		"hardfork_time_vote":                  timeString(w.HardforkTimeVote),
		"available_witness_account_subsidies": w.AvailableWitnessAccountSubsidies,
	}
}

// loadWitnesses fetches witnesses by owner name, skipping unknown ones.
func (s *Server) loadWitnesses(owners []string, legacy bool) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(owners))
	_ = s.chain.WithReadSession(func(sess *state.Session) error {
		for _, o := range owners {
			w, err := objects.GetWitness(sess, auth.AccountName(o))
			if err != nil {
				continue
			}
			out = append(out, witnessResult(w, legacy))
		}
		return nil
	})
	return out
}

// ownersFromParams decodes {"owners":[...]} or [["a","b"]].
func ownersFromParams(params json.RawMessage) ([]string, error) {
	var obj struct {
		Owners []string `json:"owners"`
	}
	if err := json.Unmarshal(params, &obj); err == nil && obj.Owners != nil {
		return obj.Owners, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(params, &arr); err == nil && len(arr) > 0 {
		var names []string
		if err := json.Unmarshal(arr[0], &names); err == nil {
			return names, nil
		}
	}
	return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "expected {\"owners\":[...]} or [[...]]")
}

func (s *Server) mFindWitnesses(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		owners, err := ownersFromParams(params)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"witnesses": s.loadWitnesses(owners, legacy)}, nil
	}
}

// mGetWitnessByAccount: condenser get_witness_by_account([name]) → witness|null.
func (s *Server) mGetWitnessByAccount(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		var arr []json.RawMessage
		if json.Unmarshal(params, &arr) != nil || len(arr) == 0 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "expected [\"account\"]")
		}
		var name string
		if json.Unmarshal(arr[0], &name) != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "account name")
		}
		ws := s.loadWitnesses([]string{name}, legacy)
		if len(ws) == 0 {
			return nil, nil
		}
		return ws[0], nil
	}
}
