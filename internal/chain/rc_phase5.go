// Package chain — RC subsystem Phase 5: per-tx cost deduction + pending
// data accumulator.
//
// This file implements the heart of the rc_plugin: pre/post-apply tx
// hooks that mirror hive::plugins::rc::rc_plugin_impl::on_*_apply_*.
//
//   1. applyRCForTx — post-apply tx: count resources, compute cost,
//      accumulate into rc_pending_data, charge the payer's manabar.
//
//   2. resetRCPendingDataForTx — pre-apply tx: clear differential_usage.
//      (Wired into the apply pipeline via direct call; per-op
//      differential visitors don't fire yet — they're scoped out of
//      Phase 5's minimum viable core.)
//
//   3. resetRCPendingDataForBlock — pre-apply block: clear pending_usage
//      + pending_cost + tx_count.
//
//   4. flushRCPendingToBucket — per-block finalize: copy pending_usage
//      into the current rc_usage_bucket's Used array.
//
// Phase 5's "minimum viable core" tracked here:
//   * count_resources(tx) for every op tag with a coverage method in
//     resource_count.cpp's count_operation_visitor.
//   * tx-level cost deduction (regen + UseMana + saturate at
//     -HIVE_RC_MAX_NEGATIVE_PERCENT * max_mana / 100).
//   * pending_data singleton accumulation + per-block flush to bucket.
//   * EnforceRC opt-in error path for live sync (insufficient_rc).
//
// Deferred (left at the "advanced tests" scope — kept as t.Skip with
// VERY specific markers):
//   * per-op differential_usage visitor (account_update / witness_update
//     / recover_account / delegate_rc).
//   * rc_stats live aggregation (add_stats per tx into the pending row).
//   * multisig signature-cost factor.

package chain

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// ErrInsufficientRC is returned by applyRCForTx when EnforceRC is on and
// the payer's regenerated manabar can't cover the tx's cost.
// Mirrors hive::not_enough_rc_exception.
var ErrInsufficientRC = errors.New("insufficient RC")

// HIVE_RC_MAX_NEGATIVE_PERCENT — the fraction of max_mana the manabar is
// allowed to go below 0. Default is 10 (10%); set as a constant.
const rcMaxNegativePercent int64 = 10

// applyRCForTx is the post-apply tx hook. Invoked from ApplyBlock after
// every op in the tx has applied successfully and BEFORE the per-tx
// savepoint is discarded. On insufficient RC (and EnforceRC=true) it
// returns ErrInsufficientRC — the caller then rolls back the tx.
//
// txSize is the on-wire signed_transaction byte size (header+ops+ext+sigs).
//
// Side effects on the supplied session:
//   * Updates rc_pending_data (creating it if it doesn't exist).
//   * Mutates payer Account (RCManaCurrent / RCManaLastUpdate).
//   * No-op if Operations is empty or the tx pays no cost (the C++
//     subsidized path).
func applyRCForTx(s *state.Session, stx *tx.SignedTransaction, blockNum, blockTime uint32, txSize int64, enforce bool) error {
	if !hardfork.SetForBlock(blockNum).Has(hardfork.HF20) {
		return nil
	}
	if len(stx.Operations) == 0 {
		return nil
	}

	// Pre-apply tx hook: clear differential_usage. We co-locate this
	// with the apply path since the pre-apply hook for diff usage runs
	// per-op in C++ but our Phase 5 core uses a single per-tx reset
	// (no per-op differential visitor yet). The reset is harmless if
	// no diff usage was accumulated.
	pending := objects.GetRCPendingData(s)
	pending.ResetDifferentialUsage()

	// 1. Count resources. Initialize result with the (negative)
	// differential_usage — the per-tx state-bytes delta from any
	// state-shrinking ops. Phase 5's minimum-viable visitor starts at
	// zero, so this is equivalent to a fresh count.
	var usage [objects.RCNumResources]int64
	CountResources(stx, txSize, &usage)

	// 2. Compute the total cost.
	cost := ComputeRCCost(stx, blockNum)
	var costPerResource [objects.RCNumResources]int64
	// Phase 5 minimum: charge the entire cost to state_bytes (the
	// dominant resource slot in practice). Full per-resource splits
	// land with the price-curve math in Phase 4 (set_pool_params).
	costPerResource[objects.RCStateBytes] = cost

	// 3. Accumulate into rc_pending_data.
	pending.AddPendingUsage(usage, costPerResource)
	objects.PutRCPendingData(s, &pending)

	// 4. Charge the payer's manabar.
	payer := PayerOf(stx.Operations[0])
	if payer == "" {
		// Tx with no resource user — C++ asserts plugin_exception. We
		// silently no-op for compat with the existing replay path.
		return nil
	}
	if cost <= 0 {
		return nil
	}
	a, err := objects.GetAccount(s, payer)
	if err != nil {
		return nil
	}
	objects.RegenerateRCMana(&a, blockTime)
	maxMana := a.MaximumRC()
	minMana := -(maxMana * rcMaxNegativePercent / 100)
	hasMana := a.RCManaCurrent >= cost
	if !hasMana && enforce {
		return fmt.Errorf("%w: account %q has %d RC, needs %d RC",
			ErrInsufficientRC, payer, a.RCManaCurrent, cost)
	}
	a.RCManaCurrent = objects.UseMana(a.RCManaCurrent, cost, minMana)
	objects.PutAccount(s, &a)
	return nil
}

// resetRCPendingDataForBlock is the pre-apply block hook. Clears the
// per-block pending totals so the new block accumulates fresh. Mirrors
// rc_plugin::on_pre_apply_block.
func resetRCPendingDataForBlock(s *state.Session) {
	pending := objects.GetRCPendingData(s)
	pending.ResetPendingUsage()
	objects.PutRCPendingData(s, &pending)
}

// flushRCPendingToBucket is the per-block finalize hook. Copies the
// accumulated rc_pending_data.PendingUsage into the current (newest)
// rc_usage_bucket's Used array, then resets pending. Mirrors the
// pool_obj.add_usage(i, block_info.usage[i]) loop in
// rc_plugin::on_post_apply_block, except we apply the additions
// directly to the active bucket row (which is what
// rotateRCUsageBuckets later sums into RCPool.UsageInWindow).
//
// Called BEFORE rotateRCUsageBuckets so the pending data lands in the
// pre-rotation newest bucket. After rotation that bucket either remains
// newest (no rotation triggered) or becomes second-newest (a fresh
// empty bucket takes the seqno+1 slot).
func flushRCPendingToBucket(s *state.Session) {
	pending := objects.GetRCPendingData(s)
	// Find the newest bucket (highest seqno).
	maxSeqno := uint32(0)
	found := false
	_ = s.Range(objects.RCUsageBucketTable, state.PrimaryIndex, nil,
		func(_, val []byte) (bool, error) {
			b, err := objects.ReadRCUsageBucket(val)
			if err != nil {
				return true, nil
			}
			if !found || b.BucketSeqno > maxSeqno {
				maxSeqno = b.BucketSeqno
				found = true
			}
			return true, nil
		})
	if found {
		newest, err := objects.GetRCUsageBucket(s, maxSeqno)
		if err == nil {
			for i := 0; i < objects.RCNumResources; i++ {
				newest.Used[i] += pending.PendingUsage[i]
			}
			objects.PutRCUsageBucket(s, &newest)
		}
	}
	pending.ResetPendingUsage()
	objects.PutRCPendingData(s, &pending)
}

// FlushRCPendingToBucketForTest exposes flushRCPendingToBucket for the
// testfixture package.
func FlushRCPendingToBucketForTest(s *state.Session) {
	flushRCPendingToBucket(s)
}

// ResetRCPendingDataForBlockForTest exposes resetRCPendingDataForBlock.
func ResetRCPendingDataForBlockForTest(s *state.Session) {
	resetRCPendingDataForBlock(s)
}

// ApplyRCForTxForTest exposes applyRCForTx.
func ApplyRCForTxForTest(s *state.Session, stx *tx.SignedTransaction, blockNum, blockTime uint32, txSize int64, enforce bool) error {
	return applyRCForTx(s, stx, blockNum, blockTime, txSize, enforce)
}
