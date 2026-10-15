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

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// ErrInsufficientRC is returned by applyRCForTx when EnforceRC is on and
// the payer's regenerated manabar can't cover the tx's cost.
// Mirrors hive::not_enough_rc_exception.
var ErrInsufficientRC = errors.New("insufficient RC")

// ResourceUserOf returns the account that pays RC for the transaction.
// Mirrors resource_credits::get_resource_user (rc/resource_user.cpp:54-62):
// visit the operations in order and take the first one that names a payer.
// Looking only at operations[0] left any transaction whose first operation
// has no payer (or an unmapped tag) uncharged — and, once RC is consensus
// under HF29, rejected.
func ResourceUserOf(stx *tx.SignedTransaction) string {
	for _, o := range stx.Operations {
		if payer := PayerOf(o); payer != "" {
			return payer
		}
	}
	return ""
}

// applyRCForTx is the post-apply tx hook. Invoked from ApplyBlock after
// every op in the tx has applied successfully and BEFORE the per-tx
// savepoint is discarded. On insufficient RC (and EnforceRC=true) it
// returns ErrInsufficientRC — the caller then rolls back the tx.
//
// txSize is the on-wire signed_transaction byte size (header+ops+ext+sigs).
//
// Side effects on the supplied session:
//   - Updates rc_pending_data (creating it if it doesn't exist).
//   - Mutates payer Account (RCManaCurrent / RCManaLastUpdate).
//   - No-op if Operations is empty or the tx pays no cost (the C++
//     subsidized path).
//
// stats is set on the block-apply paths and folds the transaction into the
// pending rc_stats (hived: is_validating_block() || is_replaying_block(),
// rc_utility.cpp:852); a production trial or admission check leaves it unset.
func applyRCForTx(s *state.Session, stx *tx.SignedTransaction, blockNum, blockTime uint32, txSize int64, enforce, stats bool, hf hardfork.Set) error {
	if !hf.Has(hardfork.HF20) {
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
	CountResources(stx, txSize, blockTime, &usage)

	// 2. Price the usage through hived's per-resource curve.
	//
	// This used to charge the raw resource count (execution nanoseconds +
	// state bytes) straight to the state_bytes slot, skipping the curve that
	// converts resource units onto the RC scale. That over-charged by two to
	// three orders of magnitude: on a loaded testnet the same account showed
	// current_mana 3,641,947,366 on hived and -141,083,244 here, and gopherd's
	// producers built near-empty blocks because every candidate was refused
	// with insufficient-rc. RC is non-consensus, so block ids matched
	// throughout and only load exposed it.
	// A zero here means the RC subsystem has nothing to price against (no
	// vesting yet, or pre-HF20), which hived treats as FREE — "when rc_regen is
	// 0, everything is free". Falling back to the old raw-resource-count cost
	// instead would silently reinstate the very over-charging this replaces.
	cost, costPerResource := rcCostOfUsageDetailed(s, usage)

	// 3. Accumulate into rc_pending_data.
	pending.AddPendingUsage(usage, costPerResource)
	objects.PutRCPendingData(s, &pending)

	info := objects.RCTxInfo{Usage: usage, Cost: costPerResource, Op: -1}
	if len(stx.Operations) == 1 {
		info.Op = int(stx.Operations[0].Tag)
	}

	// 4. Charge the payer's manabar.
	payer := ResourceUserOf(stx)
	if payer == "" {
		if stats {
			objects.AddRCStats(s, &info)
		}
		// hived HF29 (6dc8d6fd8, rc_utility.cpp:507) promoted this from a
		// NOTIFYALERT to an assertion:
		//
		//   FC_ASSERT( !db.is_in_control() && !db.has_hardfork( ..._RC_IS_CONSENSUS ),
		//     "Tried to execute transaction with no resource user" );
		//
		// Both halves are live. The in-control half used to be held back
		// because PayerOf covered only part of the operation set and this
		// would have fired on gopherd's own gaps; PayerOf now mirrors
		// get_resource_user_visitor for every operation and ResourceUserOf
		// walks the whole transaction, so a payer-less transaction here is
		// one hived refuses too (post-HF17 that is only a transaction made
		// entirely of operations with no required authority at all).
		if enforce || hf.Has(hardfork.HF29) {
			return fmt.Errorf("%w: transaction with no resource user", ErrInsufficientRC)
		}
		return nil
	}
	if cost <= 0 && !stats {
		return nil
	}
	a, err := objects.GetAccount(s, payer)
	if err != nil {
		return nil
	}
	info.Max = a.MaximumRC()
	objects.RegenerateRCMana(&a, blockTime)
	// util::manabar::use_mana (manabar.hpp:87-95) saturates at min_mana,
	// and rc_utility.cpp:604 calls it with the default, i.e. 0. There is no
	// HIVE_RC_MAX_NEGATIVE_PERCENT anywhere in hived; the 10 % negative
	// floor that used to live here let a payer's mana sink below zero during
	// replay, where hived stops at 0 — and under HF29 (RC is consensus) a
	// payer hived still considers funded would have been refused here.
	const minMana int64 = 0
	hasMana := a.RCManaCurrent >= cost
	// HF29 RC_IS_CONSENSUS (hived cbd08b3ce, rc_utility.cpp:567):
	//
	//   if( db.is_in_control() || db.is_reapplying_one_tx()
	//       || db.has_hardfork( HIVE_HARDFORK_1_29_RC_IS_CONSENSUS ) )
	//
	// Until HF29, running out of RC is enforced only where hived is in
	// control — at admission and block production — and is merely logged
	// while PROCESSING a block, so a block containing an underfunded
	// transaction still applies. `enforce` is gopherd's equivalent of those
	// first two conditions. HF29 promotes the check to consensus: every node
	// must then reject such a transaction, in a block or not.
	//
	// The gate is deliberately NOT hf29Rule-shaped. This TIGHTENS validation,
	// so it would be safe to add an InControl half — but hived's `enforce`
	// equivalents are already exactly that half, and duplicating them here
	// would enforce RC during historical replay, where a block legitimately
	// may contain a transaction the producer accepted under looser accounting.
	// Replay leaves enforce=false and HF29 inactive, so history is untouched.
	if !hasMana && (enforce || hf.Has(hardfork.HF29)) {
		return fmt.Errorf("%w: account %q has %d RC, needs %d RC",
			ErrInsufficientRC, payer, a.RCManaCurrent, cost)
	}
	a.RCManaCurrent = objects.UseMana(a.RCManaCurrent, cost, minMana)
	objects.PutAccount(s, &a)
	if stats {
		info.RC = a.RCManaCurrent
		objects.AddRCStats(s, &info)
	}
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
	_, maxSeqno, found := objects.RCUsageBucketSeqnoWindow(s)
	if found {
		newest, err := objects.GetRCUsageBucket(s, maxSeqno)
		if err == nil {
			for i := 0; i < objects.RCNumResources; i++ {
				newest.Used[i] += pending.PendingUsage[i]
			}
			objects.UpdateRCUsageBucket(s, &newest)
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

// ApplyRCForTxForTest exposes applyRCForTx. The hardfork Set is a parameter
// rather than derived from blockNum so a test can exercise HF29 RC_IS_CONSENSUS,
// which is otherwise unreachable: HF29 has no activation block, so no blockNum
// can ever produce a Set containing it.
func ApplyRCForTxForTest(s *state.Session, stx *tx.SignedTransaction, blockNum, blockTime uint32, txSize int64, enforce bool, hf hardfork.Set) error {
	return applyRCForTx(s, stx, blockNum, blockTime, txSize, enforce, true, hf)
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

// HasSufficientRC reports whether the transaction's payer can currently afford
// it, WITHOUT charging or mutating any state.
//
// This is the admission-time half of hived's RC behaviour: it enforces while
// is_in_control() (admission and block production) and not when applying
// someone else's block, since an RC-based block rejection would fork the chain.
//
// Read-only matters here — admission runs on peer receive goroutines under a
// read lock and must not write. The real charge happens once, at block build,
// in applyRCForTx; charging at admission as well would double-bill the payer.
//
// Without this check, roughly half of everything gossiped on mainnet enters the
// queue and stays: RC-insufficient transactions that every hived witness
// declines and nobody ever mines.
func HasSufficientRC(s *state.Session, stx *tx.SignedTransaction, blockNum, blockTime uint32, txSize int64) error {
	if !hardfork.SetForBlock(blockNum).Has(hardfork.HF20) || len(stx.Operations) == 0 {
		return nil
	}
	// Price the transaction the same way the charge path does. Using the raw
	// resource count here made admission two to three orders of magnitude
	// stricter than hived's, so gopherd's producers refused nearly every
	// candidate with insufficient-rc and built near-empty blocks while the
	// hived witnesses mined the same transactions.
	var usage [objects.RCNumResources]int64
	CountResources(stx, txSize, blockTime, &usage)
	cost := rcCostOfUsage(s, usage)
	if cost <= 0 {
		// RC subsystem not initialised (no vesting yet): everything is free,
		// matching hived's "when rc_regen is 0" branch.
		return nil
	}
	payer := ResourceUserOf(stx)
	if payer == "" {
		return nil
	}
	a, err := objects.GetAccount(s, payer)
	if err != nil {
		return nil // unknown payer: let the apply path produce the canonical error
	}
	// Regenerate on a COPY: this must not persist.
	objects.RegenerateRCMana(&a, blockTime)
	if a.RCManaCurrent < cost {
		return fmt.Errorf("%w: account %q has %d RC, needs %d RC",
			ErrInsufficientRC, payer, a.RCManaCurrent, cost)
	}
	return nil
}
