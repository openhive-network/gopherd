package api

import (
	"context"
	"encoding/json"

	"hived-go/internal/api/jsonrpc"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// registerListsAPI wires the account-scoped list/find reads over the typed K/V
// tables. Each does a full-table scan filtered by account (hived uses indexes;
// a scan is correct and fine for a first cut).
func (s *Server) registerListsAPI() {
	s.reg.Register("condenser_api.get_conversion_requests", s.mGetConversionRequests(true))
	s.reg.Register("condenser_api.get_collateralized_conversion_requests", s.mGetCollateralizedConversionRequests(true))
	s.reg.Register("condenser_api.get_vesting_delegations", s.mGetVestingDelegations(true))
	s.reg.Register("condenser_api.get_savings_withdraw_from", s.mGetSavingsWithdraw(true, true))
	s.reg.Register("condenser_api.get_savings_withdraw_to", s.mGetSavingsWithdraw(true, false))
	s.reg.Register("condenser_api.find_recurrent_transfers", s.mFindRecurrentTransfers(true))
	s.reg.Register("condenser_api.get_open_orders", s.mGetOpenOrders(true))
	s.reg.Register("condenser_api.get_escrow", s.mGetEscrow(true))
	s.reg.Register("condenser_api.get_owner_history", s.mGetOwnerHistory())
	s.reg.Register("condenser_api.get_recovery_request", s.mGetRecoveryRequest())
	s.reg.Register("condenser_api.list_proposals", s.mListProposals(true))
	s.reg.Register("condenser_api.find_proposals", s.mFindProposals(true))
	// database_api variants (NAI assets, {result} envelopes).
	s.reg.Register("database_api.list_proposals", s.mListProposals(false))
	s.reg.Register("database_api.find_proposals", s.mFindProposals(false))
}

// rangeTable scans every row of a table's primary index, passing the serialized
// OBJECT value (not the key) to fn. Return false to stop.
func rangeTable(sess *state.Session, table state.TableID, fn func(value []byte) bool) {
	_ = sess.Range(table, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) { return fn(value), nil })
}

// firstStringParam pulls a single account name from ["name"] or {"account":"name"}.
func firstStringParam(params json.RawMessage) string {
	var arr []json.RawMessage
	if json.Unmarshal(params, &arr) == nil && len(arr) > 0 {
		var s string
		if json.Unmarshal(arr[0], &s) == nil {
			return s
		}
	}
	var obj struct {
		Account string `json:"account"`
	}
	_ = json.Unmarshal(params, &obj)
	return obj.Account
}

func symAsset(amount int64, sym uint32, legacy bool) interface{} {
	a := asset.Asset{Amount: amount, Symbol: asset.Symbol{Num: asset.AssetNum(sym)}}
	if legacy {
		return legacyAsset(a)
	}
	return a
}

func (s *Server) mGetConversionRequests(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		acct := firstStringParam(params)
		out := []map[string]interface{}{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			rangeTable(sess, objects.ConvertRequestTable, func(body []byte) bool {
				if cr, err := objects.ReadConvertRequest(body); err == nil && string(cr.Owner) == acct {
					out = append(out, map[string]interface{}{
						"id": 0, "requestid": cr.RequestID, "owner": string(cr.Owner),
						"amount":          symAsset(cr.Amount, uint32(asset.HbdSymbol), legacy),
						"conversion_date": timeString(cr.Conversion),
					})
				}
				return true
			})
			return nil
		})
		return out, nil
	}
}

func (s *Server) mGetCollateralizedConversionRequests(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		acct := firstStringParam(params)
		out := []map[string]interface{}{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			return objects.RangeAllCollateralizedConvert(sess, func(cr objects.CollateralizedConvert) bool {
				if string(cr.Owner) == acct {
					out = append(out, map[string]interface{}{
						"id": 0, "requestid": cr.RequestID, "owner": string(cr.Owner),
						"collateral_amount": symAsset(cr.Collateral, uint32(asset.HiveSymbol), legacy),
						"converted_amount":  symAsset(cr.HBDOut, uint32(asset.HbdSymbol), legacy),
						"conversion_date":   timeString(cr.Conversion),
					})
				}
				return true
			})
		})
		return out, nil
	}
}

func (s *Server) mGetVestingDelegations(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		acct := firstStringParam(params)
		out := []map[string]interface{}{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			rangeTable(sess, objects.VestingDelegationTable, func(body []byte) bool {
				if vd, err := objects.ReadVestingDelegation(body); err == nil && string(vd.Delegator) == acct {
					out = append(out, map[string]interface{}{
						"id": 0, "delegator": string(vd.Delegator), "delegatee": string(vd.Delegatee),
						"vesting_shares":      symAsset(vd.VestingShares, uint32(asset.VestsSymbol), legacy),
						"min_delegation_time": timeString(vd.MinDelegation),
					})
				}
				return true
			})
			return nil
		})
		return out, nil
	}
}

func (s *Server) mGetSavingsWithdraw(legacy, byFrom bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		acct := firstStringParam(params)
		out := []map[string]interface{}{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			rangeTable(sess, objects.SavingsWithdrawTable, func(body []byte) bool {
				sw, err := objects.ReadSavingsWithdraw(body)
				if err != nil {
					return true
				}
				match := (byFrom && string(sw.From) == acct) || (!byFrom && string(sw.To) == acct)
				if match {
					out = append(out, map[string]interface{}{
						"id": 0, "from": string(sw.From), "to": string(sw.To), "memo": sw.Memo,
						"request_id": sw.RequestID, "amount": symAsset(sw.Amount, sw.Symbol, legacy),
						"complete": timeString(sw.Complete),
					})
				}
				return true
			})
			return nil
		})
		return out, nil
	}
}

func (s *Server) mFindRecurrentTransfers(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		acct := firstStringParam(params) // condenser: from account
		out := []map[string]interface{}{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			rangeTable(sess, objects.RecurrentTransferTable, func(body []byte) bool {
				if rt, err := objects.ReadRecurrentTransfer(body); err == nil && string(rt.From) == acct {
					out = append(out, map[string]interface{}{
						"id": 0, "from": string(rt.From), "to": string(rt.To), "memo": rt.Memo,
						"amount": symAsset(rt.Amount, rt.Symbol, legacy), "recurrence": rt.Recurrence,
						"consecutive_failures": rt.ConsecutiveFailures, "remaining_executions": rt.Executions,
						"trigger_date": timeString(rt.Trigger),
					})
				}
				return true
			})
			return nil
		})
		return out, nil
	}
}

func (s *Server) mGetOpenOrders(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		acct := firstStringParam(params)
		out := []map[string]interface{}{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			objects.EnsureLimitOrderStore(sess).RangeOrders(func(lo *objects.LimitOrder) bool {
				if string(lo.Owner) == acct {
					out = append(out, map[string]interface{}{
						"id": lo.ID, "orderid": lo.OrderID, "seller": string(lo.Owner),
						"for_sale": lo.ForSaleAmount, "created": timeString(lo.Created),
						"expiration": timeString(lo.Expiration),
						"sell_price": map[string]interface{}{
							"base":  symAsset(lo.ForSaleAmount, lo.ForSaleSymbol, legacy),
							"quote": symAsset(lo.MinReceiveAmt, lo.MinReceiveSym, legacy),
						},
					})
				}
				return true
			})
			return nil
		})
		return out, nil
	}
}

func (s *Server) mGetEscrow(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		var arr []json.RawMessage
		if json.Unmarshal(params, &arr) != nil || len(arr) < 2 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "expected [from, escrow_id]")
		}
		var from string
		var id uint32
		_ = json.Unmarshal(arr[0], &from)
		_ = json.Unmarshal(arr[1], &id)
		var out interface{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			e, err := objects.GetEscrow(sess, auth.AccountName(from), id)
			if err != nil {
				return nil
			}
			hbdSym, hiveSym := uint32(asset.HbdSymbol), uint32(asset.HiveSymbol)
			out = map[string]interface{}{
				"id": 0, "escrow_id": e.EscrowID, "from": string(e.From), "to": string(e.To),
				"hbd_balance":  symAsset(e.HBDAmount, hbdSym, legacy),
				"hive_balance": symAsset(e.HiveAmount, hiveSym, legacy),
				"pending_fee":  symAsset(e.PendingFee, ternU32(e.PendingFeeIsHBD, hbdSym, hiveSym), legacy),
				"to_approved":  e.ToApproved, "agent_approved": e.AgentApproved, "disputed": e.Disputed,
				"ratification_deadline": timeString(e.RatificationDeadline),
				"escrow_expiration":     timeString(e.EscrowExpiration),
			}
			return nil
		})
		return out, nil
	}
}

func (s *Server) mGetOwnerHistory() jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		acct := firstStringParam(params)
		out := []map[string]interface{}{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			rangeTable(sess, objects.OwnerAuthorityHistoryTable, func(body []byte) bool {
				if h, err := objects.ReadOwnerAuthorityHistory(body); err == nil && string(h.Account) == acct {
					out = append(out, map[string]interface{}{
						"id": 0, "account": string(h.Account),
						"previous_owner_authority": authorityResult(h.PreviousOwnerAuthority),
						"last_valid_time":          timeString(h.LastValidTime),
					})
				}
				return true
			})
			return nil
		})
		return out, nil
	}
}

func (s *Server) mGetRecoveryRequest() jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		acct := firstStringParam(params)
		var out interface{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			r, err := objects.GetAccountRecoveryRequest(sess, auth.AccountName(acct))
			if err != nil {
				return nil
			}
			out = map[string]interface{}{
				"id": 0, "account_to_recover": string(r.AccountToRecover),
				"expires": timeString(r.Expiration),
			}
			return nil
		})
		return out, nil
	}
}

func proposalResult(p objects.Proposal, legacy bool) map[string]interface{} {
	pay := symAsset(p.DailyPay, uint32(asset.HbdSymbol), legacy)
	return map[string]interface{}{
		"id": p.ID, "proposal_id": p.ID, "creator": string(p.Creator), "receiver": string(p.Receiver),
		"start_date": timeString(p.StartDate), "end_date": timeString(p.EndDate),
		"daily_pay": pay, "subject": p.Subject, "permlink": p.Permlink,
		"total_votes": p.TotalVotes,
	}
}

func (s *Server) mListProposals(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		limit := 1000
		var obj struct {
			Limit int `json:"limit"`
		}
		if json.Unmarshal(params, &obj) == nil && obj.Limit > 0 {
			limit = obj.Limit
		}
		if limit > 1000 { // hived clamps list queries to DATABASE_API_SINGLE_QUERY_LIMIT
			limit = 1000
		}
		out := []map[string]interface{}{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			rangeTable(sess, objects.ProposalTable, func(body []byte) bool {
				if p, err := objects.ReadProposal(body); err == nil {
					out = append(out, proposalResult(p, legacy))
				}
				return len(out) < limit
			})
			return nil
		})
		if legacy {
			return out, nil
		}
		return map[string]interface{}{"proposals": out}, nil
	}
}

func (s *Server) mFindProposals(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		var ids []int64
		var arr []json.RawMessage
		if json.Unmarshal(params, &arr) == nil && len(arr) > 0 {
			_ = json.Unmarshal(arr[0], &ids)
		} else {
			var obj struct {
				IDs []int64 `json:"proposal_ids"`
			}
			_ = json.Unmarshal(params, &obj)
			ids = obj.IDs
		}
		out := []map[string]interface{}{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			for _, id := range ids {
				if p, err := objects.GetProposal(sess, id); err == nil {
					out = append(out, proposalResult(p, legacy))
				}
			}
			return nil
		})
		if legacy {
			return out, nil
		}
		return map[string]interface{}{"proposals": out}, nil
	}
}

func ternU32(b bool, t, f uint32) uint32 {
	if b {
		return t
	}
	return f
}
