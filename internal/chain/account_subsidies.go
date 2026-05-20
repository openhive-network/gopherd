package chain

import (
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_ACCOUNT_SUBSIDY_PRECISION = HIVE_100_PERCENT = 10000.
// AvailableAccountSubsidies is tracked at this precision: 1.0 subsidy
// = 10000 internal units.
const accountSubsidyPrecision int64 = 10000

// HIVE_DEFAULT_ACCOUNT_SUBSIDY_BUDGET — default per-block production
// rate when the witness schedule hasn't elected one yet.
// Mirrors libraries/protocol/include/hive/protocol/config.hpp:
// HIVE_DEFAULT_ACCOUNT_SUBSIDY_BUDGET = 797 (= ~24/day at the median).
const defaultAccountSubsidyBudget int64 = 797

// HIVE_DEFAULT_ACCOUNT_SUBSIDY_DECAY = 347321.
const defaultAccountSubsidyDecay int64 = 347321

// rdDecayDenomShift = 36. The denominator is 2^36.
const rdDecayDenomShift = 36

// processSubsidizedAccounts mirrors database::process_subsidized_accounts
// (libraries/chain/database.cpp:1819). On every block (post-HF20):
//
//   1. Apply rd_apply to dgpo.AvailableAccountSubsidies (global pool).
//   2. Apply rd_apply to current_witness's
//      AvailableWitnessAccountSubsidies (per-witness pool).
//
// rd_apply formula: new_pool = max(pool − (pool × decay) >> 36, 0) + budget.
//
// Pre-HF20 this is a no-op.
func processSubsidizedAccounts(s *state.Session, blockNum uint32) {
	if !hardfork.SetForBlock(blockNum).Has(hardfork.HF20) {
		return
	}
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return
	}
	dgpo.AvailableAccountSubsidies = rdApply(dgpo.AvailableAccountSubsidies,
		defaultAccountSubsidyBudget, defaultAccountSubsidyDecay)
	objects.PutDGPO(s, &dgpo)

	// Per-witness pool: update the current block's producer — but ONLY
	// for elected witnesses. Mirror C++ database_witness.cpp:210 — the
	// per-witness rd_apply (update_witness_schedule_for_elected) is
	// gated on `current_witness.schedule == elected`. Timeshare/miner/
	// none-scheduled producers don't accumulate the per-witness
	// subsidy pool because they aren't supposed to be paying for
	// claim_account from that bucket. Gopherd previously ticked the
	// pool for ANY producer, inflating timeshare witnesses' per-witness
	// pool by ~40 units/block.
	if dgpo.CurrentWitness != "" {
		w, err := objects.GetWitness(s, dgpo.CurrentWitness)
		if err == nil && w.Schedule == objects.WitnessScheduleElected {
			// Per-witness budget = 5% of global, decay = 5% of global.
			// Mirrors HIVE_WITNESS_SUBSIDY_BUDGET_PERCENT / DECAY_PERCENT.
			wBudget := defaultAccountSubsidyBudget * 5 / 100
			wDecay := defaultAccountSubsidyDecay * 5 / 100
			w.AvailableWitnessAccountSubsidies = rdApply(
				w.AvailableWitnessAccountSubsidies, wBudget, wDecay)
			objects.PutWitness(s, &w)
		}
	}
}

// rdApply is the rd_apply leaky-bucket formula:
//
//	new_pool = max(pool − (pool × decay) >> 36, 0) + budget
//
// Mirrors libraries/chain/util/rd.hpp::rd_apply (the resource-dynamics
// pump used for account subsidies, RC pool decay, etc.).
func rdApply(pool, budget, decay int64) int64 {
	if pool > 0 {
		var decayed int64
		// Fast path when pool×decay fits in int64 directly.
		if pool < (1<<32) && decay < (1<<32) {
			decayed = (pool * decay) >> rdDecayDenomShift
		} else {
			// Approximate via two-step shift to avoid overflow.
			decayed = (pool >> 18) * decay >> 18
		}
		pool -= decayed
		if pool < 0 {
			pool = 0
		}
	}
	return pool + budget
}
