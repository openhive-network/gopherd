package api

import (
	"context"
	"encoding/json"

	"hived-go/internal/api/jsonrpc"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// registerCondenserAPI wires the legacy positional-args aggregator. Assets
// render as "1.234 HIVE" strings (legacy=true). Phase C so far covers the
// globals; account/witness reads land as Phase B fills in.
func (s *Server) registerCondenserAPI() {
	s.reg.Register("condenser_api.get_dynamic_global_properties", s.mGetDGPO(true))
	s.reg.Register("condenser_api.get_version", s.mGetVersion())
	s.reg.Register("condenser_api.get_feed_history", s.mGetFeedHistory(true))
	s.reg.Register("condenser_api.get_current_median_history_price", s.mGetCurrentPriceFeed(true))
	s.reg.Register("condenser_api.get_active_witnesses", s.mGetActiveWitnesses())
	s.reg.Register("condenser_api.get_witness_schedule", s.mGetWitnessSchedule())
	s.reg.Register("condenser_api.get_reward_fund", s.mCondenserGetRewardFund())
	s.reg.Register("condenser_api.get_hardfork_version", s.mCondenserHardforkVersion())
}

// mCondenserGetRewardFund: condenser positional get_reward_fund(["post"]) →
// a SINGLE fund object (not {funds:[]}), legacy assets.
func (s *Server) mCondenserGetRewardFund() jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		name := "post"
		var arr []json.RawMessage
		if json.Unmarshal(params, &arr) == nil && len(arr) > 0 {
			_ = json.Unmarshal(arr[0], &name)
		}
		var out interface{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			rf, e := objects.GetRewardFund(sess, name)
			if e == nil {
				out = rewardFundResult(rf, true)
			}
			return nil
		})
		return out, nil
	}
}

// mCondenserHardforkVersion returns the current hardfork version string.
func (s *Server) mCondenserHardforkVersion() jsonrpc.Handler {
	return func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		var v string
		if err := s.chain.WithReadSession(func(sess *state.Session) error {
			hp, e := objects.GetHardforkPropertyObject(sess)
			if e != nil {
				return e
			}
			v = "1." + itoa(hp.LastHardfork) + ".0"
			return nil
		}); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "get_hardfork_version: %v", err)
		}
		return v, nil
	}
}
