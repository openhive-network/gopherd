// Package chain — RC subsystem Phase 2 per-block hooks (HF20+/HF26+).
//
// This file implements four per-block hooks invoked from rcFinalizeBlock:
//
//   1. processRCExpiredDelegations — drains rc_expired_delegation_object
//      rows whose expiry time has passed, refunding remaining_expired
//      back to the delegator's available RC. Rate-limited at
//      HIVE_RC_MAX_ACCOUNTS_PER_DELEGATION_OP (100) entries per block per
//      delegator (mirrors C++ update_account_after_vest_change cleanup
//      pacing).
//
//   2. rotateRCUsageBuckets — HF26+ ring-buffer rotation of the 24 hourly
//      rc_usage_bucket_object rows at HIVE_RC_BUCKET_TIME_LENGTH (3600s)
//      boundaries. Sums all bucket usage into RCPool.UsageInWindow and
//      resets the rotated-out bucket.
//
//   3. updateRCPool — recomputes RCPool.PoolArray based on the per-block
//      budget minus the usage_in_window, clamped to MaxPool. Also
//      recomputes ResourceWeights (popularity shares) for RC inflation
//      allocation.
//
//   4. finalizeRCStats — at the day boundary (block_num %
//      HIVE_RC_STATS_REPORT_FREQUENCY == 0), archives the pending
//      rc_stats_object row into the archive slot and resets pending.
//
// Plus an out-of-band helper:
//
//   - handleVestingWithdrawalRCImpact — called when an account's
//     effective vesting changes (vesting withdrawal, route redirect).
//     Recomputes max_rc and, if delegations exceed the new max,
//     creates/extends an RCExpiredDelegation row to drain delegations
//     over time.
//
// Pre-HF20, all hooks are inert (the rc tables remain structurally
// empty in this window).

package chain

import (
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_RC_BUCKET_TIME_LENGTH — 3600 seconds per usage bucket (1 hour).
// HIVE_RC_WINDOW_BUCKET_COUNT — 24 buckets cover the trailing 24 hours.
const (
	rcBucketTimeLength   uint32 = 3600
	rcWindowBucketCount  uint32 = 24
	rcStatsReportFreqSec uint32 = 28800 // HIVE_RC_STATS_REPORT_FREQUENCY (8 hours)
)

// rcMaxAccountsPerDelegationOp mirrors HIVE_RC_MAX_ACCOUNTS_PER_DELEGATION_OP.
const rcMaxAccountsPerDelegationOp = 100

// processRCExpiredDelegations drains rc_expired_delegation_object rows
// whose ExpiryTime is at or before `now`. Each drained row reduces the
// owner's DelegatedRCOut by RemainingExpired (clamped at 0) and removes
// the row.
//
// Rate-limit: at most rcMaxAccountsPerDelegationOp entries are drained
// per block (across all delegators). This mirrors the C++ paced
// cleanup; in practice the queue rarely exceeds 100 in a single block
// since each delegate_rc op can produce at most that many entries.
//
// Mirrors hive::chain::rc_plugin::handle_expired_direct_delegations and
// the per-block sweep in rc_plugin.cpp on_finish_block.
func processRCExpiredDelegations(s *state.Session, now uint32) {
	type entry struct {
		row objects.RCExpiredDelegation
	}
	var batch []entry
	// Primary index of RCExpiredDelegation is keyed by account name,
	// which is unordered relative to ExpiryTime. Scan everything and
	// filter — the table is small (≤100 active entries in steady state).
	_ = s.Range(objects.RCExpiredDelegationTable, state.PrimaryIndex, nil,
		func(_, val []byte) (bool, error) {
			r, err := objects.ReadRCExpiredDelegation(val)
			if err != nil {
				return true, nil
			}
			if r.ExpiryTime > now {
				return true, nil
			}
			batch = append(batch, entry{row: r})
			// Rate-limit batch size: at most 100 per block.
			if len(batch) >= rcMaxAccountsPerDelegationOp {
				return false, nil
			}
			return true, nil
		})
	for _, e := range batch {
		acct, err := objects.GetAccount(s, e.row.From)
		if err == nil {
			refund := e.row.RemainingExpired
			if refund < 0 {
				refund = 0
			}
			acct.DelegatedRCOut -= refund
			if acct.DelegatedRCOut < 0 {
				acct.DelegatedRCOut = 0
			}
			// Bump last_max_rc to reflect newly freed delegatable RC.
			acct.LastMaxRC = acct.VestingShares - acct.DelegatedVestingShares +
				acct.ReceivedVestingShares - acct.DelegatedRCOut + acct.DelegatedRCIn
			if acct.LastMaxRC < 0 {
				acct.LastMaxRC = 0
			}
			objects.RegenerateRCMana(&acct, now)
			objects.PutAccount(s, &acct)
		}
		objects.DeleteRCExpiredDelegation(s, e.row.From)
	}
}

// rotateRCUsageBuckets advances the rolling 24-hour bucket window when
// block timestamps cross a bucket boundary (every 3600 seconds).
//
// On each rotation:
//   1. The oldest bucket (max_seqno - 23) is removed.
//   2. A new bucket with seqno = max_seqno + 1 is created with Used=0.
//   3. RCPool.UsageInWindow is recomputed by summing all surviving
//      buckets' Used arrays.
//
// At genesis (no buckets exist yet) we seed RCWindowBucketCount empty
// buckets so the math is well-formed.
//
// Mirrors hive::chain::rc_plugin::on_finish_block bucket rotation +
// rc_pool_object::recalculate_resource_weights's usage-window scan.
func rotateRCUsageBuckets(s *state.Session, now uint32) {
	// Inspect the current set of buckets. We track:
	//   - maxSeqno: the newest bucket's seqno
	//   - newestCreated: the newest bucket's Created timestamp
	//   - count: total number of bucket rows
	maxSeqno := uint32(0)
	newestCreated := uint32(0)
	count := 0
	_ = s.Range(objects.RCUsageBucketTable, state.PrimaryIndex, nil,
		func(_, val []byte) (bool, error) {
			b, err := objects.ReadRCUsageBucket(val)
			if err != nil {
				return true, nil
			}
			if count == 0 || b.BucketSeqno > maxSeqno {
				maxSeqno = b.BucketSeqno
				newestCreated = b.Created
			}
			count++
			return true, nil
		})

	if count == 0 {
		// Seed RCWindowBucketCount empty buckets, all marked Created=now.
		// Their seqno space is [1..RCWindowBucketCount].
		for i := uint32(1); i <= rcWindowBucketCount; i++ {
			objects.PutRCUsageBucket(s, &objects.RCUsageBucket{
				BucketSeqno: i,
				Created:     now,
			})
		}
		return
	}

	// Should we rotate? Only when the newest bucket is at least
	// HIVE_RC_BUCKET_TIME_LENGTH old.
	if now < newestCreated+rcBucketTimeLength {
		return
	}

	// Rotate: drop oldest, add newest. The oldest seqno is the smallest;
	// since we keep exactly RCWindowBucketCount buckets, oldest =
	// maxSeqno - RCWindowBucketCount + 1.
	var oldestSeqno uint32
	if maxSeqno >= rcWindowBucketCount {
		oldestSeqno = maxSeqno - rcWindowBucketCount + 1
	} else {
		oldestSeqno = 1
	}
	objects.DeleteRCUsageBucket(s, oldestSeqno)
	objects.PutRCUsageBucket(s, &objects.RCUsageBucket{
		BucketSeqno: maxSeqno + 1,
		Created:     now,
	})

	// Recompute RCPool.UsageInWindow as the sum across surviving buckets.
	if !objects.HasRCPool(s) {
		return
	}
	pool, err := objects.GetRCPool(s)
	if err != nil {
		return
	}
	var sums [objects.RCNumResources]int64
	_ = s.Range(objects.RCUsageBucketTable, state.PrimaryIndex, nil,
		func(_, val []byte) (bool, error) {
			b, err := objects.ReadRCUsageBucket(val)
			if err != nil {
				return true, nil
			}
			for i := 0; i < objects.RCNumResources; i++ {
				sums[i] += b.Used[i]
			}
			return true, nil
		})
	pool.UsageInWindow = sums
	objects.PutRCPool(s, &pool)
}

// updateRCPool recomputes each pool's available capacity:
//
//	pool[i] = clamp( pool[i] + budget[i] - usage_in_window[i] / WINDOW_BUCKETS,
//	                 0, max_pool[i] )
//
// and recomputes resource_weights as popularity shares of UsageInWindow.
// When SumOfResourceWeights would be zero (genesis / cold pool) we fall
// back to equal weighting so per-resource inflation never divides by zero.
//
// Mirrors hive::chain::rc_pool_object::recalculate_resource_weights and
// the per-block pool top-up in rc_plugin.cpp on_finish_block.
func updateRCPool(s *state.Session, now uint32) {
	if !objects.HasRCPool(s) {
		return
	}
	pool, err := objects.GetRCPool(s)
	if err != nil {
		return
	}
	// Resource-weight recompute: each resource's basis-point share of
	// UsageInWindow, summed into SumOfResourceWeights.
	totalUsage := int64(0)
	for i := 0; i < objects.RCNumResources; i++ {
		if pool.UsageInWindow[i] > 0 {
			totalUsage += pool.UsageInWindow[i]
		}
	}
	if totalUsage > 0 {
		var sum uint64
		for i := 0; i < objects.RCNumResources; i++ {
			share := uint16(0)
			if pool.UsageInWindow[i] > 0 {
				// basis-points: usage[i] * 10000 / totalUsage
				share = uint16(pool.UsageInWindow[i] * 10000 / totalUsage)
			}
			pool.ResourceWeights[i] = share
			sum += uint64(share)
		}
		if sum == 0 {
			sum = 1
		}
		pool.SumOfResourceWeights = sum
	} else {
		// Equal weighting fallback.
		for i := 0; i < objects.RCNumResources; i++ {
			pool.ResourceWeights[i] = 2000 // 1/5 in basis points
		}
		pool.SumOfResourceWeights = uint64(objects.RCNumResources) * 2000
	}

	// Per-block budget top-up: pool[i] += budget[i] (clamped to max_pool[i]).
	// LastKnownBudget mirrors the per-resource budget from the witness-
	// voted params. Pre-HF20 this is zero so the top-up is a no-op.
	for i := 0; i < objects.RCNumResources; i++ {
		// Decay component: subtract a 1/24-hour share of usage_in_window
		// (one bucket's worth) — approximation of C++'s rd_decay_params.
		decay := pool.UsageInWindow[i] / int64(rcWindowBucketCount)
		pool.PoolArray[i] += pool.LastKnownBudget[i] - decay
		// Clamp [0, legacy.Max]. Pre-HF20 the legacy block carries the
		// active max for compatibility with initRCPoolIfNeeded.
		maxP := pool.Pool[i].Max
		if maxP <= 0 {
			maxP = (1 << 62)
		}
		if pool.PoolArray[i] > maxP {
			pool.PoolArray[i] = maxP
		}
		if pool.PoolArray[i] < 0 {
			pool.PoolArray[i] = 0
		}
		// Track for legacy callers + writes a stable update timestamp.
		pool.Pool[i].Current = pool.PoolArray[i]
		pool.Pool[i].Update = now
	}
	pool.LastRecalcTime = now
	objects.PutRCPool(s, &pool)
}

// finalizeRCStats archives the pending rc_stats_object row into the
// archive slot at the day boundary. The pending row is then reset to
// zero so the next reporting window starts fresh.
//
// Trigger: blockNum > 0 && blockNum % (HIVE_RC_STATS_REPORT_FREQUENCY /
// HIVE_BLOCK_INTERVAL) == 0 — i.e. once every 9600 blocks (3-sec
// interval × 28800 sec). We approximate with blockNum %
// (rcStatsReportFreqSec/3) == 0 to match the C++ pacing.
//
// Mirrors hive::chain::rc_plugin::on_finish_block stats archival.
func finalizeRCStats(s *state.Session, blockNum uint32) {
	const blocksPerReport = rcStatsReportFreqSec / 3 // 3-sec block interval
	if blockNum == 0 || blockNum%blocksPerReport != 0 {
		return
	}
	pending, err := objects.GetRCStats(s, objects.RCStatsIDPending)
	if err != nil {
		// Nothing to archive (table empty at first crossing).
		return
	}
	// Promote pending → archive.
	archive := pending
	archive.BlockNum = blockNum
	objects.PutRCStats(s, objects.RCStatsIDArchive, &archive)
	// Reset pending. Preserve the running Stamp so per-day chains link.
	stamp := pending.Stamp
	objects.PutRCStats(s, objects.RCStatsIDPending, &objects.RCStats{
		BlockNum: blockNum,
		Stamp:    stamp,
	})
}

// handleVestingWithdrawalRCImpact is called when an account's effective
// vesting decreases (a power-down chunk withdraws, or a withdraw_route
// redirects shares). It recomputes the account's max_rc; if delegations
// now exceed the new max, the over-delegated amount is "expired" — an
// RCExpiredDelegation row is created (or extended) and the account's
// DelegatedRCOut is reduced. The expired row drains back to the
// delegator over rcDelegationReturnPeriodSec (5 days).
//
// Caller responsibility: invoke this AFTER mutating VestingShares
// (or ReceivedVestingShares / DelegatedVestingShares) and BEFORE
// PutAccount. The account's LastMaxRC is updated in place.
//
// Mirrors C++ rc_plugin::update_account_after_vest_change: when a vest
// reduction makes delegated_rc > new_max_delegable_rc, the surplus is
// moved into the rc_expired_delegation_object and the direct_delegation
// rows are clipped (oldest-first or proportionally).
func handleVestingWithdrawalRCImpact(s *state.Session, a *objects.Account, now uint32) {
	// Delegates to the canonical implementation in state/objects so the
	// evaluator package (which can't import chain) can call it via the
	// same path. See objects.HandleVestingWithdrawalRCImpact.
	objects.HandleVestingWithdrawalRCImpact(s, a, now)
}

// Exported wrappers for test-only access from sibling packages
// (internal/chain/testfixture). These do not change package boundaries
// — the unexported hooks remain the canonical entry points.

// ProcessRCExpiredDelegationsForTest drains expired RC delegations.
// Test-only; production callers use rcFinalizeBlockPhase2.
func ProcessRCExpiredDelegationsForTest(s *state.Session, now uint32) {
	processRCExpiredDelegations(s, now)
}

// RotateRCUsageBucketsForTest rotates the hourly usage buckets.
// Test-only; see processRCExpiredDelegations for the production path.
func RotateRCUsageBucketsForTest(s *state.Session, now uint32) {
	rotateRCUsageBuckets(s, now)
}

// UpdateRCPoolForTest recomputes the RC pool. Test-only.
func UpdateRCPoolForTest(s *state.Session, now uint32) {
	updateRCPool(s, now)
}

// FinalizeRCStatsForTest archives the pending stats row. Test-only.
func FinalizeRCStatsForTest(s *state.Session, blockNum uint32) {
	finalizeRCStats(s, blockNum)
}

// HandleVestingWithdrawalRCImpactForTest clips delegations after a
// vesting reduction. Test-only.
func HandleVestingWithdrawalRCImpactForTest(s *state.Session, a *objects.Account, now uint32) {
	handleVestingWithdrawalRCImpact(s, a, now)
}

// rcFinalizeBlockPhase2 wires the four hooks above into the per-block
// finalize sequence. Called from rcFinalizeBlock (which retains the
// existing pre-HF20 fast-skip).
func rcFinalizeBlockPhase2(s *state.Session, blockNum, blockTime uint32) {
	processRCExpiredDelegations(s, blockTime)
	// Phase 5 bridge: flush rc_pending_data.PendingUsage into the
	// active rc_usage_bucket BEFORE rotation, so the data the block's
	// txs accumulated lands in the pre-rotation newest bucket.
	if hardfork.SetForBlock(blockNum).Has(hardfork.HF26) {
		flushRCPendingToBucket(s)
		rotateRCUsageBuckets(s, blockTime)
		updateRCPool(s, blockTime)
	}
	finalizeRCStats(s, blockNum)
}
