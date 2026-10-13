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
	"gopherd/internal/chain/evaluator"
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
	// One visit per interpreted operation: hived runs the differential from
	// on_pre_apply_custom_operation, which fires for each delegate_rc of a
	// batched payload.
	ops, err := evaluator.ParseRCCustomJSON(cj.JSON, false)
	if err != nil {
		return diff
	}
	for i := range ops {
		diff[objects.RCStateBytes] += delegateRCDifferential(s, &ops[i])
	}
	return diff
}

func delegateRCDifferential(s *state.Session, p *evaluator.DelegateRCParams) int64 {
	if p.MaxRC == 0 {
		return 0
	}
	if has, _ := objects.HasAccount(s, auth.AccountName(p.From)); !has {
		return 0
	}
	var usage int64
	for _, to := range p.Delegatees {
		if has, _ := objects.HasAccount(s, auth.AccountName(to)); !has {
			// C++ returns false (no diff) if any delegatee missing.
			return 0
		}
		// Existing delegation row contributes its full size to diff.
		if _, err := objects.GetRCDirectDelegation(s,
			auth.AccountName(p.From), auth.AccountName(to)); err == nil {
			usage += rcDirectDelegationNodeSize * lastingStateByte
		}
	}
	return usage
}
