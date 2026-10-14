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
//   2.+3. finalizeRCPoolsForBlock — resource_credits::finalize_block: pool
//      dynamics, the 24 hourly rc_usage_bucket rows and the resource
//      weights, every block from HF20.
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
	"math/bits"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// HIVE_RC_BUCKET_TIME_LENGTH — 3600 seconds per usage bucket (1 hour).
// HIVE_RC_WINDOW_BUCKET_COUNT — 24 buckets cover the trailing 24 hours.
const (
	rcBucketTimeLength  uint32 = 3600
	rcWindowBucketCount uint32 = 24
	// HIVE_RC_STATS_REPORT_FREQUENCY is HIVE_BLOCKS_PER_DAY = 28800 BLOCKS
	// (config.hpp:488; rc_utility.cpp:701 compares block numbers), not seconds.
	// Dividing it by the block interval archived every 9600 blocks instead.
	rcStatsReportFreqBlocks uint32 = 28800
)

// processRCExpiredDelegations is resource_credits::handle_expired_delegations,
// run every block from HF26, before the pools are finalized (database.cpp:2370).
// See objects.ProcessRCExpiredDelegations.
func processRCExpiredDelegations(s *state.Session, now uint32) {
	objects.ProcessRCExpiredDelegations(s, now)
}

// finalizeRCPoolsForBlock ports resource_credits::finalize_block
// (rc/rc_utility.cpp:765-830), the per-block update of the RC resource
// pools, their rolling 24-hour usage window and the resource weights that
// split RC inflation between resources. Runs every block from HF20 on, as
// hived's does.
//
// Per resource i, with `usage` this block's consumption scaled to pool
// units (compute_cost multiplies tx usage by resource_unit before it is
// added to block_info, rc_utility.cpp:318):
//
//	decay     = rd_compute_pool_decay( params.decay, pool - usage, 1 )
//	window   -= oldest bucket           (only when the hour rolled over)
//	window   += usage
//	new_pool  = pool - decay + budget - usage,   clamped at 0
//	new_pool  = dgpo.available_account_subsidies  for new_accounts
//	weight_i  = window_i * 10000 / budget_i  (min 1; new_accounts = sum)
//
// then the block's usage is added to the active hourly bucket, which is
// the OLDEST bucket reset and reused when the newest is an hour old.
//
// The previous implementation diverged on every line of this: it never
// subtracted the block's usage from the pool, decayed by window/24 instead
// of rd_compute_pool_decay, clamped at a maximum hived does not have,
// never pinned new_accounts to the subsidy pool, computed weights as shares
// of total usage instead of usage over budget, pooled UNscaled usage, and
// refreshed the window only once an hour. Every RC price gopherd quoted
// therefore drifted from hived's even with identical usage — harmless
// while RC is advisory, a split once HF29 makes it consensus.
func finalizeRCPoolsForBlock(s *state.Session, now uint32) {
	if !objects.HasRCPool(s) {
		return
	}
	pool, err := objects.GetRCPool(s)
	if err != nil {
		return
	}
	params, err := objects.GetRCResourceParam(s)
	if err != nil {
		return
	}
	pending := objects.GetRCPendingData(s)

	// Buckets: the newest (highest seqno) is active; by_timestamp rbegin()
	// in hived. When it is a full HIVE_RC_BUCKET_TIME_LENGTH old, the OLDEST
	// bucket is reset and becomes the new active one.
	// Oldest and newest bucket, as hived reads them off by_timestamp
	// (rc_utility.cpp:777-781): two point reads keyed by the seqno window.
	// Scanning the primary rows instead walked every pebble version of the
	// active bucket, which is rewritten each block (88% of the apply loop
	// after HF20 on a mainnet sync).
	var newest, oldest objects.RCUsageBucket
	have := false
	if oSeq, nSeq, ok := objects.RCUsageBucketSeqnoWindow(s); ok {
		o, oerr := objects.GetRCUsageBucket(s, oSeq)
		n, nerr := objects.GetRCUsageBucket(s, nSeq)
		if oerr == nil && nerr == nil {
			oldest, newest, have = o, n, true
		}
	}
	if !have {
		// First block with RC active: seed the window (hived creates the
		// HIVE_RC_WINDOW_BUCKET_COUNT buckets at HF20 bootstrap).
		for i := uint32(1); i <= rcWindowBucketCount; i++ {
			b := objects.RCUsageBucket{BucketSeqno: i, Created: now}
			objects.PutRCUsageBucket(s, &b)
			if i == 1 {
				oldest = b
			}
			newest = b
		}
	}
	reset := newest.Created+rcBucketTimeLength <= now
	active := newest
	if reset {
		active = oldest
	}

	var usage [objects.RCNumResources]int64
	for i := 0; i < objects.RCNumResources; i++ {
		usage[i] = pending.PendingUsage[i] * int64(params.Entries[i].Dynamics.ResourceUnit)
	}

	dgpoRef, haveDGPO := objects.GetDGPORef(s)
	for i := 0; i < objects.RCNumResources; i++ {
		dyn := params.Entries[i].Dynamics
		p := pool.PoolArray[i]
		budget := int64(dyn.BudgetPerTime)
		pool.LastKnownBudget[i] = budget
		decay := rcPoolDecay(dyn, p-usage[i])
		if reset {
			pool.UsageInWindow[i] -= active.Used[i]
		}
		pool.UsageInWindow[i] += usage[i]
		newPool := p - decay + budget - usage[i]
		if i == int(objects.RCNewAccounts) && haveDGPO {
			// The consensus account-subsidy pool is the authority for this
			// resource; the RC copy is re-pinned to it every block.
			newPool = dgpoRef.AvailableAccountSubsidies
		}
		if newPool < 0 {
			newPool = 0
		}
		pool.PoolArray[i] = newPool
		// Legacy shim fields read by older callers.
		pool.Pool[i].Current = newPool
		pool.Pool[i].Update = now
	}
	// rc_pool_object::recalculate_resource_weights (rc_objects.hpp:81-95).
	var sum uint64
	for i := 0; i < objects.RCNumResources; i++ {
		if i == int(objects.RCNewAccounts) {
			continue
		}
		var w uint64
		if budget := int64(params.Entries[i].Dynamics.BudgetPerTime); budget > 0 && pool.UsageInWindow[i] > 0 {
			w = uint64(pool.UsageInWindow[i] * 10000 / budget)
		}
		if w == 0 {
			w = 1 // minimum weight
		}
		pool.ResourceWeights[i] = w
		sum += w
	}
	pool.ResourceWeights[objects.RCNewAccounts] = sum // always 100%
	pool.SumOfResourceWeights = sum
	pool.LastRecalcTime = now
	objects.PutRCPool(s, &pool)

	if reset {
		// bucket.reset( now ): contents were already subtracted above. The
		// reused bucket takes the next seqno so "newest" stays the highest.
		objects.DeleteRCUsageBucket(s, active.BucketSeqno)
		active = objects.RCUsageBucket{BucketSeqno: newest.BucketSeqno + 1, Created: now}
	}
	for i := 0; i < objects.RCNumResources; i++ {
		active.Used[i] += usage[i]
	}
	if reset {
		objects.PutRCUsageBucket(s, &active) // new bucket: row + index entry
	} else {
		objects.UpdateRCUsageBucket(s, &active)
	}

	pending.ResetPendingUsage()
	objects.PutRCPendingData(s, &pending)
}

// rcPoolDecay is util::rd_compute_pool_decay (util/rd.cpp:23-39) with dt = 1
// and the resource's own denominator shift:
//
//	decay = ( u128(decay_per_time_unit) * pool ) >> shift,  capped at pool
//
// negative pools decay symmetrically.
func rcPoolDecay(dyn objects.ResourceDynamics, pool int64) int64 {
	if pool < 0 {
		return -rcPoolDecay(dyn, -pool)
	}
	hi, lo := bits.Mul64(uint64(dyn.DecayPerTime), uint64(pool))
	shift := uint(dyn.DecayDenomShift)
	var result uint64
	switch {
	case shift == 0:
		result = lo
	case shift >= 64:
		result = hi >> (shift - 64)
	default:
		result = hi<<(64-shift) | lo>>shift
	}
	if result > uint64(pool) {
		return pool
	}
	return int64(result)
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
	const blocksPerReport = rcStatsReportFreqBlocks
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
	finalizeRCPoolsForBlock(s, now)
}

// UpdateRCPoolForTest recomputes the RC pool. Test-only.
func UpdateRCPoolForTest(s *state.Session, now uint32) {
	finalizeRCPoolsForBlock(s, now)
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
	if hardfork.SetForBlock(blockNum).Has(hardfork.HF26) {
		processRCExpiredDelegations(s, blockTime)
	}
	// resource_credits::finalize_block runs from HF20 (rc_utility.cpp:767),
	// not HF26: the pools, window and weights evolve continuously from the
	// RC bootstrap. Gating this on HF26 left gopherd.s pools parked at
	// pool_eq for the six hardforks in between.
	finalizeRCPoolsForBlock(s, blockTime)
	finalizeRCStats(s, blockNum)
}
