package api

import (
	"context"
	"encoding/json"
	"strconv"

	"hived-go/internal/api/jsonrpc"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

func witnessResult(w objects.Witness, legacy bool) map[string]interface{} {
	fee := func(x int64) interface{} {
		if legacy {
			return legacyAsset(hiveAsset(x))
		}
		return hiveAsset(x)
	}
	return map[string]interface{}{
		"owner":                  string(w.Owner),
		"created":                timeString(w.Created),
		"url":                    w.URL,
		"votes":                  strconv.FormatInt(w.TotalVotesVests, 10),
		"virtual_last_update":    u128dec(w.VirtualLastUpdate),
		"virtual_position":       u128dec(w.VirtualPosition),
		"virtual_scheduled_time": u128dec(w.VirtualScheduledTime),
		"total_missed":           w.TotalMissedBlocks,
		"last_confirmed_block_num": w.LastConfirmedBlockNum,
		"pow_worker":             w.PowWorker,
		"signing_key":            w.BlockSigningKey.String(),
		"props": map[string]interface{}{
			"account_creation_fee": fee(w.AccountCreationFee),
			"maximum_block_size":   w.MaximumBlockSize,
			"hbd_interest_rate":    w.HBDInterestRate,
			"account_subsidy_budget": w.AccountSubsidyBudget,
			"account_subsidy_decay":  w.AccountSubsidyDecay,
		},
		"hbd_exchange_rate":        priceResult(w.HBDExchangeRate, legacy),
		"last_hbd_exchange_update": timeString(w.LastHBDExchangeUpdate),
		"running_version":          versionString(w.RunningVersion),
		"hardfork_version_vote":    versionString(w.HardforkVersionVote),
		"hardfork_time_vote":       timeString(w.HardforkTimeVote),
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
