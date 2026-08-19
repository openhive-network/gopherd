// Package chain — RC subsystem hooks (HF20+).
//
// The active replay window stops at block ~2.9M, far below the HF20
// activation block (~26.2M), so the per-block work below is dormant
// in current usage. Implementations are scaffolding: faithful in
// shape, with the heavier per-resource decay math left for the HF20
// extension milestone (memory: project_rc_subsystem_deferred).
package chain

import (
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
	"math"
)

// HIVE_RC_DELEGATION_RETURN_PERIOD = 5 days. Matches
// HIVE_VOTING_MANA_REGENERATION_SECONDS — a reduced delegation refunds
// to the delegator over the same window the manabar regenerates.
const rcDelegationReturnPeriodSec uint32 = 5 * 24 * 60 * 60

// initRCPoolIfNeeded creates the rc_pool_object singleton on the first
// call after HF20. Called from set_pool_params during update_witness_schedule.
func initRCPoolIfNeeded(s *state.Session, blockTime uint32) {
	if objects.HasRCPool(s) {
		return
	}
	var pool objects.RCPool
	for i := 0; i < objects.RCNumResources; i++ {
		pool.Pool[i] = objects.RCPoolEntry{
			Current: 0, Max: 0, Update: blockTime, Cost: 1,
		}
	}
	objects.PutRCPool(s, &pool)
}

// rcSetPoolParams ports rc_plugin::set_pool_params (rc_plugin.cpp:330).
// Re-derives per-resource pool maxima from the witness-voted dynamic
// params + median chain_properties. Called from update_witness_schedule
// after the median is rolled.
func rcSetPoolParams(s *state.Session, blockNum uint32, blockTime uint32) {
	if !hardfork.SetForBlock(blockNum).Has(hardfork.HF20) {
		return
	}
	initRCPoolIfNeeded(s, blockTime)
	applyAccountSubsidyRDToRC(s)
}

// applyAccountSubsidyRDToRC replaces the new_accounts resource's dynamics with
// the witness-voted account-subsidy parameters and REGENERATES its price curve
// from them, mirroring resource_credits::set_pool_params (rc_utility.cpp:858).
//
// new_accounts is the one resource whose parameters do not come from
// resource_parameters.json: the jsonball entry is a placeholder ("this pool is
// recalculated with account subsidy witness params") whose budget rounds to
// zero per block. Leaving it that way gave a pool of 0 and coeff_b of 0, so
// the cost function divided by nothing and returned ZERO — gopherd charged no
// RC at all for claim_account, where hived charges heavily. That is an
// under-charge, i.e. a free-account spam path, and the only place gopherd was
// more permissive than hived.
func applyAccountSubsidyRDToRC(s *state.Session) {
	global, _ := subsidyRDParams(s)
	if global.budget <= 0 || global.decay <= 0 || global.maxPool <= 0 {
		return
	}
	params, err := objects.GetRCResourceParam(s)
	if err != nil {
		return
	}
	e := &params.Entries[objects.RCNewAccounts]

	budget := global.budget
	if budget > math.MaxInt32 {
		budget = math.MaxInt32
	}
	decay := global.decay
	if decay > math.MaxUint32 {
		decay = math.MaxUint32
	}
	next := objects.ResourceDynamics{
		// resource_unit is a property of the resource, not of the witness
		// params, so it carries over from the jsonball entry.
		ResourceUnit:    e.Dynamics.ResourceUnit,
		BudgetPerTime:   int32(budget),
		PoolEq:          uint64(global.maxPool),
		MaxPool:         uint64(global.maxPool),
		DecayPerTime:    uint32(decay),
		DecayDenomShift: rdDecayDenomShift,
	}
	if e.Dynamics == next {
		return
	}
	e.Dynamics = next
	// new_accounts uses a=37/1000, u=1/2 (resource_parameters.json).
	e.Curve = generateRCCurveParams(&next, 37, 1000, 1, 2)
	objects.PutRCResourceParam(s, &params)

	// The pool's per-block budget and ceiling are cached from whatever the
	// params said at bootstrap, which for new_accounts is the zero placeholder.
	// Refresh them, or the pool never accumulates: it sat at 0 forever while
	// hived's climbed at 797/block, leaving the denominator at coeff_b and
	// account creation permanently at maximum price.
	//
	// The POOL LEVEL itself is deliberately left alone — hived's
	// set_pool_params only rewrites params, so the pool keeps filling from
	// wherever it was rather than jumping to the new equilibrium.
	if pool, perr := objects.GetRCPool(s); perr == nil {
		i := int(objects.RCNewAccounts)
		if pool.LastKnownBudget[i] != int64(next.BudgetPerTime) || pool.Pool[i].Max != int64(next.MaxPool) {
			pool.LastKnownBudget[i] = int64(next.BudgetPerTime)
			pool.Pool[i].Max = int64(next.MaxPool)
			objects.PutRCPool(s, &pool)
		}
	}
}

// rcFinalizeBlock ports rc_plugin::on_finish_block. Per-block hooks:
//
//  1. Account for produced/consumed RC since the last block.
//  2. Decay each pool's current_mana toward its max.
//  3. Process expired RC delegations (refund to delegator).
//  4. Update the rc_per_resource cost coefficients.
//
// Scaffolding pre-HF20: only #3 fires (it's safe to invoke even before
// HF20 activates since the table is empty). The cost-recompute math is
// driven by witness-voted dynamic_params, which are HF20+ only.
func rcFinalizeBlock(s *state.Session, blockNum, blockTime uint32) {
	if !hardfork.SetForBlock(blockNum).Has(hardfork.HF20) {
		// Pre-HF20: the RC delegation expiration table is structurally
		// empty (delegate_rc lands HF20+) so the per-block expiration
		// sweep can't fire. Skip entirely to kill the ~210 MB cum spent
		// in the empty Range path.
		return
	}
	rcHandleExpiredDelegations(s, blockTime)
	rcSetPoolParams(s, blockNum, blockTime)
	// Phase 2 hooks: expired-delegation drain, bucket rotation, pool
	// recompute, stats archival. These are inert pre-HF20 (the early
	// fast-skip above handles that) and become meaningful HF26+.
	rcFinalizeBlockPhase2(s, blockNum, blockTime)
}

// rcHandleExpiredDelegations sweeps rc_delegation_expiration_object rows
// whose expiration ≤ now, refunding their RC to the delegator's
// available pool. Mirrors handle_expired_delegations (rc_plugin.cpp:558).
func rcHandleExpiredDelegations(s *state.Session, now uint32) {
	type expired struct {
		row objects.RCDelegationExpiration
	}
	var batch []expired
	_ = s.Range(objects.RCDelegationExpirationTable,
		objects.RCDelegationExpirationByExpirationIndex, nil,
		func(key, _ []byte) (bool, error) {
			if len(key) < 4+8 {
				return true, nil
			}
			exp := uint32(key[0])<<24 | uint32(key[1])<<16 |
				uint32(key[2])<<8 | uint32(key[3])
			if exp > now {
				return false, nil // index is ASC; first non-expired stops the scan
			}
			seq := uint64(key[4])<<56 | uint64(key[5])<<48 | uint64(key[6])<<40 | uint64(key[7])<<32 |
				uint64(key[8])<<24 | uint64(key[9])<<16 | uint64(key[10])<<8 | uint64(key[11])
			row, err := objects.GetRCDelegationExpiration(s, seq)
			if err != nil {
				return true, nil
			}
			batch = append(batch, expired{row: row})
			return true, nil
		})
	for _, e := range batch {
		// Refund: bump the delegator's max_rc back up by row.RC and
		// regenerate the manabar so the freed mana is immediately
		// available.
		delegator, err := objects.GetAccount(s, e.row.Delegator)
		if err == nil {
			delegator.DelegatedRCOut -= int64(e.row.RC)
			if delegator.DelegatedRCOut < 0 {
				delegator.DelegatedRCOut = 0
			}
			objects.RegenerateRCMana(&delegator, now)
			objects.PutAccount(s, &delegator)
		}
		objects.DeleteRCDelegationExpiration(s, &e.row)
	}
}

// GetRCDelegationExpiration fetches a single row by seq. Provided here
// (next to the other RC-package-private helpers) since the storage object
// file doesn't include a fetch helper.
//
// (deferred until needed by callers; rcHandleExpiredDelegations uses the
// indexed range path.)
var _ = func() {} // package-doc anchor

// QueueRCDelegationExpiration enqueues a pending refund row for the
// supplied delegator. Used by the rc_custom delegate_rc handler when a
// delegation is reduced or removed.
func QueueRCDelegationExpiration(s *state.Session, delegator auth.AccountName, rc uint64, now uint32) {
	if rc == 0 {
		return
	}
	seq := nextRCDelegationExpirationSeq(s)
	objects.PutRCDelegationExpiration(s, &objects.RCDelegationExpiration{
		Seq:        seq,
		Delegator:  delegator,
		RC:         rc,
		Expiration: now + rcDelegationReturnPeriodSec,
	})
}

// nextRCDelegationExpirationSeq derives a fresh monotonic seq id by
// scanning the current max in the table. The N is small (≤ 5d ×
// max_per_block delegations); a simple scan is fine until a counter
// field is added to DGPO.
func nextRCDelegationExpirationSeq(s *state.Session) uint64 {
	max := uint64(0)
	_ = s.Range(objects.RCDelegationExpirationTable, state.PrimaryIndex, nil,
		func(key, _ []byte) (bool, error) {
			if len(key) < 8 {
				return true, nil
			}
			seq := uint64(key[0])<<56 | uint64(key[1])<<48 |
				uint64(key[2])<<40 | uint64(key[3])<<32 |
				uint64(key[4])<<24 | uint64(key[5])<<16 |
				uint64(key[6])<<8 | uint64(key[7])
			if seq > max {
				max = seq
			}
			return true, nil
		})
	return max + 1
}
