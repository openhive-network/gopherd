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
// Phase 5.1 landed (this file + rc_differential.go):
//   * per-op differential_usage visitor (account_update /
//     account_update2 / witness_update / recover_account / delegate_rc).
//   * pre-apply tx hook resets differential; pre-apply OP hook
//     accumulates; post-apply tx hook (applyRCForTx) seeds CountResources
//     with the accumulated differential so the net is (new − old).
//
// Deferred (left at the "advanced tests" scope):
//   * rc_stats live aggregation (add_stats per tx into the pending row).
//   * multisig signature-cost factor.

package chain

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
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

	// Phase 5.1: load pending. DifferentialUsage was reset at pre-apply
	// tx and accumulated negatively across per-op visitors in the apply
	// loop (see applyRCDifferentialForOp). We initialize the count_resources
	// result FROM the accumulated differential — when CountResources
	// then adds the new full state_bytes, the net is (new − old).
	pending := objects.GetRCPendingData(s)

	// 1. Count resources. Initialize result with the (negative)
	// differential_usage from the per-op visitor pass — CountResources
	// will add the new state_bytes on top.
	usage := pending.DifferentialUsage
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

// resetRCDifferentialForTx is the pre-apply tx hook (HF20+). Clears the
// per-tx differential_usage accumulator before any op fires its
// pre-apply-op visitor. Mirrors rc_plugin_impl::on_pre_apply_transaction
// (rc_plugin.cpp:346-355).
func resetRCDifferentialForTx(s *state.Session, blockNum uint32) {
	if !hardfork.SetForBlock(blockNum).Has(hardfork.HF20) {
		return
	}
	pending := objects.GetRCPendingData(s)
	pending.ResetDifferentialUsage()
	objects.PutRCPendingData(s, &pending)
}

// applyRCDifferentialForOp is the pre-apply op hook (HF20+). Computes
// the per-op differential usage via countDifferentialOp and accumulates
// it (negated) into rc_pending_data.DifferentialUsage. Mirrors the
// prepare_differential_usage + add_differential_usage pair in
// rc_plugin_impl::on_pre_apply_operation (rc_plugin.cpp:1121-1126).
func applyRCDifferentialForOp(s *state.Session, o op.Op, blockNum uint32) {
	if !hardfork.SetForBlock(blockNum).Has(hardfork.HF20) {
		return
	}
	diff := countDifferentialOp(s, o)
	// Skip the persist round-trip when nothing accumulated (the common
	// case for transfers, votes, comments, …).
	empty := true
	for i := 0; i < objects.RCNumResources; i++ {
		if diff[i] != 0 {
			empty = false
			break
		}
	}
	if empty {
		return
	}
	pending := objects.GetRCPendingData(s)
	pending.AddDifferentialUsage(diff)
	objects.PutRCPendingData(s, &pending)
}

// ResetRCDifferentialForTxForTest exposes resetRCDifferentialForTx.
func ResetRCDifferentialForTxForTest(s *state.Session, blockNum uint32) {
	resetRCDifferentialForTx(s, blockNum)
}

// ApplyRCDifferentialForOpForTest exposes applyRCDifferentialForOp.
func ApplyRCDifferentialForOpForTest(s *state.Session, o op.Op, blockNum uint32) {
	applyRCDifferentialForOp(s, o, blockNum)
}
