// Package chain — RC subsystem Phase 5.1: per-op differential resource
// usage visitor.
//
// Mirrors hive::plugins::rc::count_differential_operation_visitor in
// libraries/plugins/rc/resource_count.cpp.
//
// For some operations (account_update, account_update2, recover_account,
// witness_update, delegate_rc), the marginal state cost charged to the
// payer should only reflect the DELTA between the new and OLD state, not
// the full state footprint of the new object. The C++ rc plugin
// implements this by:
//
//   1. Pre-apply OP: read the EXISTING state object (account_authority,
//      witness, rc_direct_delegation) — record its current state_bytes
//      footprint, accumulated NEGATIVELY into rc_pending_data.
//      differential_usage.
//
//   2. The op then applies, producing the new state_bytes footprint
//      computed by count_operation_visitor.
//
//   3. Post-apply TX: count_resources is initialized with the (already
//      negative) differential_usage. When the post-apply count adds the
//      new full state_bytes, the net is delta = new - old.
//
// Pre-apply TX resets differential_usage to 0, so the per-tx delta is
// fresh for every transaction.
//
// Resources affected: only state_bytes. history_bytes, new_accounts,
// market_bytes, execution_time are never set by the differential
// visitor — those stay at the tx-level count_resources output.

package chain

import (
	"encoding/json"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// countDifferentialOp returns the per-resource differential usage for op
// `o` evaluated against the CURRENT state in session `s`.
//
// The returned values are positive — callers accumulate them into
// rc_pending_data.DifferentialUsage via AddDifferentialUsage, which
// negates the sum (mirroring C++ rc_pending_data::add_differential_usage).
//
// Mirrors count_differential_operation_visitor::operator() overloads.
func countDifferentialOp(s *state.Session, o op.Op) [objects.RCNumResources]int64 {
	var diff [objects.RCNumResources]int64
	switch o.Tag {
	case op.TagAccountUpdate:
		au := o.AsAccountUpdate()
		authRow, err := objects.GetAccountAuthority(s, au.Account)
		if err != nil {
			return diff
		}
		var usage int64
		if au.Owner != nil {
			usage += authorityDynamicSizeReal(&authRow.Owner)
		}
		if au.Active != nil {
			usage += authorityDynamicSizeReal(&authRow.Active)
		}
		if au.Posting != nil {
			usage += authorityDynamicSizeReal(&authRow.Posting)
		}
		// metadata not handled (see C++ comment — depends on COLLECT_ACCOUNT_METADATA)
		diff[objects.RCStateBytes] = usage
		return diff

	case op.TagAccountUpdate2:
		au := o.AsAccountUpdate2()
		authRow, err := objects.GetAccountAuthority(s, au.Account)
		if err != nil {
			return diff
		}
		var usage int64
		if au.Owner != nil {
			usage += authorityDynamicSizeReal(&authRow.Owner)
		}
		if au.Active != nil {
			usage += authorityDynamicSizeReal(&authRow.Active)
		}
		if au.Posting != nil {
			usage += authorityDynamicSizeReal(&authRow.Posting)
		}
		diff[objects.RCStateBytes] = usage
		return diff

	case op.TagRecoverAccount:
		ra := o.AsRecoverAccount()
		authRow, err := objects.GetAccountAuthority(s, ra.AccountToRecover)
		if err != nil {
			return diff
		}
		// "if transaction qualifies for subsidy old state usage will be
		// ignored" — we always compute it; the subsidy clamp in
		// CountResources zeroes the result for qualifying txs.
		diff[objects.RCStateBytes] = authorityDynamicSizeReal(&authRow.Owner)
		return diff

	case op.TagWitnessUpdate:
		wu := o.AsWitnessUpdate()
		w, err := objects.GetWitness(s, wu.Owner)
		if err != nil {
			return diff
		}
		diff[objects.RCStateBytes] =
			witnessIndexNodeSize*persistentStateByte +
				witnessUpdateURLCharSize*int64(len(w.URL))
		return diff

	case op.TagCustomJSON:
		// delegate_rc is carried as a custom_json with id == "rc".
		return countDifferentialCustomJSON(s, o)
	}
	return diff
}

// authorityDynamicSizeReal computes the dynamic-size component of an
// authority using ITS ACTUAL members (not the flat 200-byte estimate
// used by authorityDynamicSize for op-side counting). Mirrors C++
// count_differential_operation_visitor::get_authority_dynamic_size:
//
//	size = authority_account_member_size * |account_auths|
//	     + authority_key_member_size     * |key_auths|
func authorityDynamicSizeReal(a *auth.Authority) int64 {
	return authorityAccountMemberSize*int64(len(a.AccountAuths)) +
		authorityKeyMemberSize*int64(len(a.KeyAuths))
}

// countDifferentialCustomJSON handles custom_json ops with id == "rc"
// that wrap a delegate_rc payload. For each delegatee that already has
// an rc_direct_delegation row, the existing per-row state cost is added
// to the differential.
//
// Mirrors count_differential_operation_visitor::operator()
// (delegate_rc_operation): max_rc == 0 → no diff; otherwise for each
// (from, to) pair where a delegation row exists, add delegate_rc_base_size.
func countDifferentialCustomJSON(s *state.Session, o op.Op) [objects.RCNumResources]int64 {
	var diff [objects.RCNumResources]int64
	cj := o.AsCustomJSON()
	if cj.ID != "rc" {
		return diff
	}
	from, delegatees, maxRC, ok := parseDelegateRCPayload(cj.JSON)
	if !ok || maxRC == 0 {
		return diff
	}
	if has, _ := objects.HasAccount(s, auth.AccountName(from)); !has {
		return diff
	}
	var usage int64
	for _, to := range delegatees {
		if has, _ := objects.HasAccount(s, auth.AccountName(to)); !has {
			// C++ returns false (no diff) if any delegatee missing.
			return [objects.RCNumResources]int64{}
		}
		// Existing delegation row contributes its full size to diff.
		if _, err := objects.GetRCDirectDelegation(s,
			auth.AccountName(from), auth.AccountName(to)); err == nil {
			usage += rcDirectDelegationNodeSize * lastingStateByte
		}
	}
	diff[objects.RCStateBytes] = usage
	return diff
}

// parseDelegateRCPayload is a tolerant JSON parser for the delegate_rc
// payload format. Mirrors applyDelegateRC's input handling — both the
// object form and the tuple form are accepted.
//
// The outer wrapper is ["delegate_rc", <inner>] or [0, <inner>]. The
// inner is either {"from":..., "delegatees":[...], "max_rc":N}, or
// the tuple [from, [delegatees], max_rc, {}].
func parseDelegateRCPayload(payload string) (from string, delegatees []string, maxRC int64, ok bool) {
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(payload), &raw); err != nil || len(raw) != 2 {
		return "", nil, 0, false
	}
	// Verify name == "delegate_rc" or 0
	var name string
	if err := json.Unmarshal(raw[0], &name); err != nil {
		var n int
		if err2 := json.Unmarshal(raw[0], &n); err2 != nil || n != 0 {
			return "", nil, 0, false
		}
	} else if name != "delegate_rc" {
		return "", nil, 0, false
	}
	// Try object form
	var p struct {
		From       string   `json:"from"`
		Delegatees []string `json:"delegatees"`
		MaxRC      int64    `json:"max_rc"`
	}
	if err := json.Unmarshal(raw[1], &p); err == nil && p.From != "" {
		return p.From, p.Delegatees, p.MaxRC, true
	}
	// Tuple form: [from, [delegatees], max_rc, {}]
	var tup []json.RawMessage
	if err := json.Unmarshal(raw[1], &tup); err != nil || len(tup) < 3 {
		return "", nil, 0, false
	}
	if err := json.Unmarshal(tup[0], &p.From); err != nil {
		return "", nil, 0, false
	}
	if err := json.Unmarshal(tup[1], &p.Delegatees); err != nil {
		return "", nil, 0, false
	}
	if err := json.Unmarshal(tup[2], &p.MaxRC); err != nil {
		return "", nil, 0, false
	}
	return p.From, p.Delegatees, p.MaxRC, true
}
