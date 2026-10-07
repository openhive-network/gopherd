// Package chain — RC subsystem Phase 5 per-tx resource counting.
//
// Mirrors hive::plugins::rc::count_resources in
// libraries/plugins/rc/resource_count.cpp.
//
// The result is the raw resource_count_type ([5]int64) used by the per-tx
// pre-apply / post-apply hooks. The five resource slots are:
//
//	0  resource_history_bytes
//	1  resource_new_accounts
//	2  resource_market_bytes
//	3  resource_state_bytes
//	4  resource_execution_time
//
// count_resources also implements the "single subsidized op" rule:
// when a transaction contains exactly one recover_account_operation with
// a single-key/single-account new_owner_authority and at most 2
// signatures, the transaction is RC-free (history_bytes/market_bytes
// remain 0).

package chain

import (
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
	"gopherd/internal/state/objects"
)

// transactionBaseStateBytes mirrors state_object_size_info::transaction_base_size
// (resource_sizes.cpp:120-122): SIZE(transaction_index node, 128) ×
// TEMPORARY_STATE_BYTE × 1, "multiply by signature expiration time (rounded
// up to full hours)". The multiplication happens in count_resources
// (resource_count.cpp:621-623); a flat 256 was charged here before.
const transactionBaseStateBytes int64 = 128

// transactionExpirationHours is resource_count.cpp:621:
//
//	( tx.expiration - now + 3600 - 1 ) / 3600
//
// in uint32 arithmetic. A transaction is only counted after it passed the
// expiration check, so expiration > now and the result is at least 1;
// clamp at 0 for callers that count a synthetic transaction.
func transactionExpirationHours(expiration, now uint32) int64 {
	if expiration <= now {
		return 0
	}
	return (int64(expiration) - int64(now) + 3600 - 1) / 3600
}

// rcCountVisitor collects per-op resource counts. Mirrors C++
// count_operation_visitor's mutable accumulators.
type rcCountVisitor struct {
	marketOpCount        int32
	newAccountOpCount    int32
	stateBytesCount      int64
	executionTimeCount   int64
	subsidizedOp         bool
	subsidizedSignatures uint32
}

// visit accumulates op `o` into v. Mirrors count_operation_visitor's
// per-op overloads.
func (v *rcCountVisitor) visit(o op.Op, now uint32) {
	// state_bytes — hived's table, case by case.
	v.stateBytesCount += stateBytesForOp(o, now)

	// execution_time — the per-op table plus the pieces hived adds outside
	// it (recurrent_transfer executions, delegate_rc).
	if t, ok := execTimeNS[o.Tag]; ok {
		v.executionTimeCount += t
	}
	v.executionTimeCount += extraExecTimeForOp(o)

	// market_op flag — these match the C++ visitor's market_op_count++.
	switch o.Tag {
	case op.TagTransfer,
		op.TagTransferToVesting,
		op.TagLimitOrderCreate,
		op.TagLimitOrderCreate2,
		op.TagRecurrentTransfer:
		v.marketOpCount++
	}

	// new_account op tracking — claim_account with fee==0 (claims a
	// subsidized account) increments new_accounts.
	switch o.Tag {
	case op.TagClaimAccount:
		if o.AsClaimAccount().Fee.Amount == 0 {
			v.newAccountOpCount++
		}
	}

	// Subsidized-op detection: recover_account with a single-auth
	// new_owner_authority and ≤ 2 signatures pays no RC.
	if o.Tag == op.TagRecoverAccount {
		ra := o.AsRecoverAccount()
		nm := uint32(len(ra.NewOwnerAuthority.AccountAuths) + len(ra.NewOwnerAuthority.KeyAuths))
		v.subsidizedOp = (nm == 1)
		v.subsidizedSignatures = 2
	}
}

// CountResources mirrors hive::plugins::rc::count_resources(signed_transaction).
//
// txSize is the on-wire byte size of the signed transaction; numSignatures
// is len(tx.Signatures). When the transaction qualifies for the
// "single subsidized op" rule, the result is left at the differential
// usage (clamped at 0) — no history/market_bytes, no per-tx state base,
// no execution time. Otherwise the per-resource sum mirrors the C++
// formula exactly.
//
// The initial value of result is preserved (used for the per-tx
// differential_usage initialization).
//
// now is hived's `now` argument to count_resources: head_block_time() at the
// moment the transaction is charged. It only feeds the per-transaction state
// charge, which scales with how many whole hours remain until expiration.
func CountResources(stx *tx.SignedTransaction, txSize int64, now uint32, result *[objects.RCNumResources]int64) {
	var v rcCountVisitor
	for _, o := range stx.Operations {
		v.visit(o, now)
	}
	subsidized := v.subsidizedOp &&
		len(stx.Operations) == 1 &&
		uint32(len(stx.Signatures)) <= v.subsidizedSignatures
	if subsidized {
		// Transaction is RC-free. C++ clamps the (already-negative)
		// differential_usage to 0 and returns.
		for i := 0; i < objects.RCNumResources; i++ {
			if result[i] < 0 {
				result[i] = 0
			}
		}
		return
	}

	result[objects.RCHistoryBytes] += txSize
	result[objects.RCNewAccounts] += int64(v.newAccountOpCount)
	if v.marketOpCount > 0 {
		result[objects.RCMarketBytes] += txSize
	}
	result[objects.RCStateBytes] += v.stateBytesCount +
		transactionBaseStateBytes*transactionExpirationHours(stx.Expiration, now)
	// per-tx execution time includes the base + signature verification.
	verifyTime := verifyAuthorityTimeNS * int64(len(stx.Signatures))
	result[objects.RCExecutionTime] += v.executionTimeCount + transactionExecTimeNS + verifyTime

	// Clamp negatives — differential_usage initialization could leave
	// us with a final negative on a single resource if no op rebuilt
	// the cost; C++ prevent_negative() applies the same clamp.
	for i := 0; i < objects.RCNumResources; i++ {
		if result[i] < 0 {
			result[i] = 0
		}
	}
}

// CountResourcesForTx is a convenience wrapper that allocates a fresh
// result slice. Tests use this; production callers initialize result
// from the (negated) differential_usage on rc_pending_data and reuse it.
func CountResourcesForTx(stx *tx.SignedTransaction, txSize int64, now uint32) [objects.RCNumResources]int64 {
	var result [objects.RCNumResources]int64
	CountResources(stx, txSize, now, &result)
	return result
}
