package chain

import (
	"math"
	"math/bits"

	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_ACCOUNT_SUBSIDY_PRECISION = HIVE_100_PERCENT = 10000.
// AvailableAccountSubsidies is tracked at this precision: 1.0 subsidy
// = 10000 internal units.
const accountSubsidyPrecision int64 = 10000

// HIVE_DEFAULT_ACCOUNT_SUBSIDY_BUDGET / DECAY — chain_properties
// defaults (config.hpp:417-419). Every witness row carries these until
// it publishes custom values via witness_set_properties (HF20+), so
// the schedule medians equal them for the whole pre-HF20 era.
const (
	defaultAccountSubsidyBudget int64 = 797
	defaultAccountSubsidyDecay  int64 = 347321
)

// rdDecayDenomShift = HIVE_RD_DECAY_DENOM_SHIFT = 36.
const rdDecayDenomShift = 36

// Per-witness rd params scale off the medians (config.hpp:409-414):
// budget × 125%, decay × 2100% (21 witnesses share the global budget,
// so each one's bucket refills slightly faster and decays 21× harder).
const (
	witnessSubsidyBudgetPercentBP int64 = 12500  // HIVE_WITNESS_SUBSIDY_BUDGET_PERCENT
	witnessSubsidyDecayPercentBP  int64 = 210000 // HIVE_WITNESS_SUBSIDY_DECAY_PERCENT (21×10000)
	decayBackstopPercentBP        int64 = 9000   // HIVE_DECAY_BACKSTOP_PERCENT
)

// rdParams mirrors util::rd_dynamics_params — the resolved leaky-bucket
// parameters hived computes at every schedule rebuild
// (witness_schedule.cpp:108-144) and stores in the wso. All inputs are
// the schedule's stored medians, so re-deriving them per block yields
// the values of the latest rebuild.
type rdParams struct {
	budget   int64
	decay    int64 // decay rate per time unit (>> 36 denominator)
	maxPool  int64 // pool_eq = ceil((budget << 36) / decay)
	minDecay int64 // witness-pool backstop; 0 for the global pool
}

// rdComputePoolDecay mirrors util::rd_compute_pool_decay with dt=1:
// decay = (u128(decay_rate) × pool) >> 36, capped at pool.
func rdComputePoolDecay(decayRate, pool int64) int64 {
	if pool < 0 {
		return -rdComputePoolDecay(decayRate, -pool)
	}
	hi, lo := bits.Mul64(uint64(decayRate), uint64(pool))
	result := hi<<(64-rdDecayDenomShift) | lo>>rdDecayDenomShift
	if result > uint64(pool) {
		return pool
	}
	return int64(result)
}

// rdApply mirrors util::rd_apply (util/rd.cpp:42-53):
//
//	decay    = max(rd_compute_pool_decay(pool), min_decay)
//	new_pool = clamp(pool + budget − decay, 0, max_pool_size)
func rdApply(p rdParams, pool int64) int64 {
	decay := rdComputePoolDecay(p.decay, pool)
	if decay < p.minDecay {
		decay = p.minDecay
	}
	newPool := pool + p.budget - decay
	if newPool > p.maxPool {
		newPool = p.maxPool
	}
	if newPool < 0 {
		newPool = 0
	}
	return newPool
}

// rdPoolEq mirrors rd_setup_dynamics_params' pool_eq:
// ceil((budget << 36) / decay), computed 128-bit (budget can reach
// int32 max on the witness side after the 125% scale).
func rdPoolEq(budget, decay int64) int64 {
	if decay <= 0 {
		return math.MaxInt64
	}
	hi, lo := bits.Mul64(uint64(budget), 1<<rdDecayDenomShift)
	lo2, carry := bits.Add64(lo, uint64(decay-1), 0)
	hi += carry
	if hi >= uint64(decay) {
		return math.MaxInt64
	}
	q, _ := bits.Div64(hi, lo2, uint64(decay))
	if q > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(q)
}

// subsidyRDParams derives (global, per-witness) rd params from the
// witness schedule's stored medians, mirroring witness_schedule.cpp:
// 108-144 including the per-witness 125%/2100% scaling, int32/uint32
// clamps, pool_eq caps, and the 90% min-decay backstop evaluated at the
// median per-witness pool level captured at rebuild.
func subsidyRDParams(s *state.Session) (global, witness rdParams) {
	budget := defaultAccountSubsidyBudget
	decay := defaultAccountSubsidyDecay
	medianAvail := int64(0)
	if ws, ok := objects.GetWitnessScheduleRef(s); ok {
		if ws.MedianAccountSubsidyBudget > 0 {
			budget = int64(ws.MedianAccountSubsidyBudget)
		}
		if ws.MedianAccountSubsidyDecay > 0 {
			decay = int64(ws.MedianAccountSubsidyDecay)
		}
		medianAvail = ws.MedianAvailableWitnessAccountSubsidies
	}
	global = rdParams{budget: budget, decay: decay, maxPool: rdPoolEq(budget, decay)}

	wBudget := budget * witnessSubsidyBudgetPercentBP / 10000
	if wBudget > math.MaxInt32 {
		wBudget = math.MaxInt32
	}
	wDecay := decay * witnessSubsidyDecayPercentBP / 10000
	if wDecay > math.MaxUint32 {
		wDecay = math.MaxUint32
	}
	witness = rdParams{budget: wBudget, decay: wDecay, maxPool: rdPoolEq(wBudget, wDecay)}
	medianDecay := rdComputePoolDecay(wDecay, medianAvail)
	if medianDecay < 0 {
		medianDecay = 0
	}
	witness.minDecay = medianDecay * decayBackstopPercentBP / 10000
	return global, witness
}

// processSubsidizedAccounts mirrors database::process_subsidized_accounts
// (database.cpp:1865-1879). It runs UNCONDITIONALLY from genesis — NOT
// HF20-gated: hived fills both the global pool and the per-witness
// pools for the whole pre-HF20 era. The global pool is zeroed at HF20
// activation, but the per-witness pools are NOT — elected witnesses
// arrive at HF20 with their buckets already at pool_eq (~937.97
// accounts), which is what funds the first fee-0 claim_account ops.
func processSubsidizedAccounts(s *state.Session, blockNum uint32) {
	globalRD, witnessRD := subsidyRDParams(s)

	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return
	}
	dgpo.AvailableAccountSubsidies = rdApply(globalRD, dgpo.AvailableAccountSubsidies)
	objects.PutDGPO(s, &dgpo)

	// Per-witness pool: update the current block's producer, ONLY when
	// elected (update_witness_schedule_for_elected,
	// database_witness.cpp:207-217).
	if dgpo.CurrentWitness != "" {
		w, err := objects.GetWitness(s, dgpo.CurrentWitness)
		if err == nil && w.Schedule == objects.WitnessScheduleElected {
			w.AvailableWitnessAccountSubsidies = rdApply(witnessRD, w.AvailableWitnessAccountSubsidies)
			objects.PutWitness(s, &w)
		}
	}
}
