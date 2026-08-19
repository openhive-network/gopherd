package chain

import (
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// HF20 RC bootstrap — ports database_hardfork.cpp:355-403 piecewise.
// The block of state pieces created at HIVE_HARDFORK_0_20 activation:
//
//  1. rc_resource_param_object (singleton) — per-resource dynamics +
//     price-curve params parsed from libraries/jsonball/data/
//     resource_parameters.json.
//  2. rc_pool_object (singleton) — initialized with pool_array = pool_eq
//     for each resource and resource_weights summing to HIVE_100_PERCENT.
//  3. 24 × rc_usage_bucket_object with staggered Created timestamps so
//     the first rotation happens at the natural hour boundary.
//  4. 2 × rc_stats_object rows (RC_PENDING_STATS_ID + RC_ARCHIVE_STATS_ID).
//  5. WitnessSchedule.AccountSubsidyRd + AccountSubsidyWitnessRd (the
//     rd_dynamics_params struct, derived from set_pool_params via
//     witness median chain_properties).
//
// The account-level rc_adjustment / current_mana / last_max_rc seeding
// already lives in hardforks.go (HF20 case) — this file fills in the
// rest of the C++ HF20 init block.

// hiveBlockInterval — HIVE_BLOCK_INTERVAL = 3 seconds. Used to convert
// JSON time durations (days/hours/seconds) into block counts.
//
// rdDecayDenomShift (36) lives in account_subsidies.go.
const hiveBlockInterval uint32 = 3

// jsonballResource captures the 5 per-resource entries from
// libraries/jsonball/data/resource_parameters.json in their literal form
// so we don't drag in a JSON parser at runtime. Field semantics mirror
// the JSON keys; the values here are byte-identical to the C++ source.
type jsonballResource struct {
	// budgetTimeSeconds is "budget_time" converted to seconds (e.g. 30d
	// → 2,592,000).
	budgetTimeSeconds uint64
	// budget is the raw "budget" value from JSON. Pre-multiplied by
	// resource_unit before per-block conversion (see rd_setup math).
	budget uint64
	// halfLifeSeconds is "half_life" converted to seconds.
	halfLifeSeconds uint64
	// resourceUnit = resource_unit_base ** resource_unit_exponent.
	resourceUnit uint64
	// Price-curve a-point and u-point — see rc_price_curve_params.
	aPointNum, aPointDenom uint64
	uPointNum, uPointDenom uint64
}

// canonicalRCResources contains the per-resource jsonball params as they
// ship in libraries/jsonball/data/resource_parameters.json. The order
// MUST match the RCResource enum (RCHistoryBytes, RCNewAccounts,
// RCMarketBytes, RCStateBytes, RCExecutionTime).
var canonicalRCResources = [objects.RCNumResources]jsonballResource{
	// resource_history_bytes — Cost of network transport + block_log storage.
	{
		budgetTimeSeconds: 30 * 24 * 60 * 60, // 30 days
		budget:            37_500_000_000,
		halfLifeSeconds:   15 * 24 * 60 * 60, // 15 days
		resourceUnit:      1,                 // 10^0
		aPointNum:         150,
		aPointDenom:       10000,
		uPointNum:         1,
		uPointDenom:       2,
	},
	// resource_new_accounts — controlled by witnesses post-init.
	{
		budgetTimeSeconds: 30000, // 30000 seconds
		budget:            797,
		halfLifeSeconds:   5 * 24 * 60 * 60, // 5 days
		resourceUnit:      10000,            // 10^4
		aPointNum:         37,
		aPointDenom:       1000,
		uPointNum:         1,
		uPointDenom:       2,
	},
	// resource_market_bytes.
	{
		budgetTimeSeconds: 30 * 24 * 60 * 60,
		budget:            6_250_000_000,
		halfLifeSeconds:   16 * 60 * 60, // 16 hours
		resourceUnit:      10,           // 10^1
		aPointNum:         150,
		aPointDenom:       10000,
		uPointNum:         1,
		uPointDenom:       2,
	},
	// resource_state_bytes — state RAM cost.
	{
		budgetTimeSeconds: 30 * 24 * 60 * 60,
		budget:            37_623_913_512_960,
		halfLifeSeconds:   15 * 24 * 60 * 60,
		resourceUnit:      1,
		aPointNum:         150,
		aPointDenom:       10000,
		uPointNum:         1,
		uPointDenom:       2,
	},
	// resource_execution_time.
	{
		budgetTimeSeconds: 60 * 60, // 1 hour
		budget:            48_000_000_000,
		halfLifeSeconds:   60 * 60,
		resourceUnit:      1,
		aPointNum:         150,
		aPointDenom:       10000,
		uPointNum:         1,
		uPointDenom:       2,
	},
}

// computeDecayPerTimeUnit returns floor(2^36 * (1 - 2^(-1/H))) — the
// per-block decay constant such that the pool halves every H blocks.
//
// We avoid float math: 1 - 2^(-1/H) ≈ ln(2)/H for large H (>> 36).
// For the values we care about (H in [1200, 432000]), the error of the
// linear approximation is < 1 ULP at the scale of 2^36 because the
// remaining series terms are O((ln 2)^2 / H^2) and 2^36 * (ln 2)^2 /
// 1200^2 ≈ 18 — close to but consistently > 0; we accept the upper
// bound. C++ uses a precise computation; we match by:
//
//	decay ≈ floor(2^36 * ln(2) / H)
//
// with ln(2) approximated as 0xB17217F7D1CF79AB / 2^64 — IEEE 754
// double-precision representation of ln(2). The constant gives sub-ULP
// accuracy across the H range that appears in canonicalRCResources.
func computeDecayPerTimeUnit(halfLifeBlocks uint64) uint32 {
	if halfLifeBlocks == 0 {
		return 0
	}
	// 2^36 * ln(2) = 47,666,688,419 (rounded from 47666688418.9...)
	const twoPow36Ln2 uint64 = 47_666_688_419
	d := twoPow36Ln2 / halfLifeBlocks
	if d > 1<<32-1 {
		d = 1<<32 - 1
	}
	return uint32(d)
}

// initRCBootstrapHF20 writes the rc_resource_param singleton, the
// rc_pool singleton with pool_array = pool_eq per resource, the 24
// staggered rc_usage_buckets, and the 2 rc_stats rows. Idempotent —
// pre-existing rows are not overwritten so re-running HF20 is safe.
//
// Called from processHardforks at the HF20 case.
func initRCBootstrapHF20(s *state.Session, blockTime uint32) {
	// 1. rc_resource_param_object.
	var rp objects.RCResourceParam
	for i := 0; i < objects.RCNumResources; i++ {
		r := canonicalRCResources[i]
		budgetTimeBlocks := r.budgetTimeSeconds / uint64(hiveBlockInterval)
		halfLifeBlocks := r.halfLifeSeconds / uint64(hiveBlockInterval)
		// budget_per_time_unit per block, with the budget scaled by
		// resource_unit for EVERY resource.
		//
		// This previously skipped the scaling, on the belief that hived applied
		// it only to new_accounts. It does not: market_bytes has
		// resource_unit 10, and a live hived node reports its pool as
		// 2,001,842,832 against gopherd's 200,209,445 — exactly 10x. The
		// resource_unit also multiplies usage at cost time, so missing it here
		// shrank the pool and inflated the price by the same factor.
		// history_bytes, state_bytes and execution_time all have
		// resource_unit 1, which is why only market_bytes visibly diverged.
		budgetPerTime := (r.budget * r.resourceUnit) / budgetTimeBlocks
		if i == int(objects.RCNewAccounts) {
			// new_accounts is the exception: its jsonball entry is an explicit
			// placeholder ("this pool is recalculated with account subsidy
			// witness params") and hived does NOT scale it, so the per-block
			// budget truncates to zero and the POOL starts empty, filling at
			// the witness-voted subsidy rate once set_pool_params installs the
			// real dynamics.
			//
			// Scaling it here instead gave pool_eq 165,457,537 and a pool that
			// started FULL, which made claim_account cheap — under-charging
			// against hived, whose pool reads 13*797 a few blocks in.
			budgetPerTime = r.budget / budgetTimeBlocks
		}
		if budgetPerTime > (1<<31 - 1) {
			budgetPerTime = 1<<31 - 1
		}
		decayPerTime := computeDecayPerTimeUnit(halfLifeBlocks)
		// pool_eq = ceil((budget << 36) / decay) — derived in
		// rd_setup_dynamics_params (chain/util/rd.cpp:88).
		poolEq := uint64(0)
		if decayPerTime > 0 {
			// (budgetPerTime << 36) / decayPerTime, rounded up.
			num := budgetPerTime << 36
			d := uint64(decayPerTime)
			poolEq = (num + d - 1) / d
		}
		rp.Entries[i] = objects.RCResourceEntry{
			Dynamics: objects.ResourceDynamics{
				ResourceUnit:    r.resourceUnit,
				BudgetPerTime:   int32(budgetPerTime),
				PoolEq:          poolEq,
				MaxPool:         poolEq,
				DecayPerTime:    decayPerTime,
				DecayDenomShift: uint8(rdDecayDenomShift),
				MinDecay:        0,
			},
			// Price curve, generated exactly as hived does
			// (generate_rc_curve_params, rc_curve.cpp:33).
			//
			// This used to be a placeholder — coeff_a = pool_eq*a_num/a_denom,
			// coeff_b = pool_eq*u_num/u_denom, shift = 0 — which is not the
			// generator at all. Against a live hived testnet the state_bytes
			// curve read coeff_a=10525659774662010880 coeff_b=212030656091
			// shift=50, where gopherd had 406810620401 / 13560354013374 / 0.
			// The coefficients set the RC PRICE, so gopherd over-charged by
			// orders of magnitude, drove every account to the -10% mana floor
			// and produced near-empty blocks under load.
			Curve: generateRCCurveParams(&objects.ResourceDynamics{
				PoolEq:          poolEq,
				DecayPerTime:    decayPerTime,
				DecayDenomShift: uint8(rdDecayDenomShift),
			}, r.aPointNum, r.aPointDenom, r.uPointNum, r.uPointDenom),
		}
	}
	if !objects.HasRCResourceParam(s) {
		objects.PutRCResourceParam(s, &rp)
	}

	// 2. rc_pool_object — pool_array seeded to pool_eq, weights split
	// uniformly into 5 × 2000 bp = 10000. C++ initializes weights via
	// rc_pool_object's constructor with all set to HIVE_100_PERCENT /
	// HIVE_RC_NUM_RESOURCE_TYPES (1/5 = 2000 bp).
	if !objects.HasRCPool(s) {
		var pool objects.RCPool
		for i := 0; i < objects.RCNumResources; i++ {
			pool.PoolArray[i] = int64(rp.Entries[i].Dynamics.PoolEq)
			pool.LastKnownBudget[i] = int64(rp.Entries[i].Dynamics.BudgetPerTime)
			pool.ResourceWeights[i] = 2000
			// Legacy block stays in sync so the older PoolEntry shape is
			// still readable until callers migrate.
			pool.Pool[i] = objects.RCPoolEntry{
				Current: int64(rp.Entries[i].Dynamics.PoolEq),
				Max:     int64(rp.Entries[i].Dynamics.MaxPool),
				Update:  blockTime,
				Cost:    1,
			}
		}
		pool.SumOfResourceWeights = 5 * 2000
		pool.LastRecalcTime = blockTime
		objects.PutRCPool(s, &pool)
	}

	// 3. 24 staggered rc_usage_buckets — Created timestamps stride back
	// 1h each so the first rotation lands at the natural hour boundary.
	// Mirrors database_hardfork.cpp:379-384.
	for i := uint32(0); i < rcWindowBucketCount; i++ {
		seqno := i + 1
		// Skip if a bucket with this seqno already exists (idempotent
		// re-runs of HF20).
		if _, err := objects.GetRCUsageBucket(s, seqno); err == nil {
			continue
		}
		ago := (rcWindowBucketCount - 1 - i) * rcBucketTimeLength
		created := uint32(0)
		if blockTime > ago {
			created = blockTime - ago
		}
		objects.PutRCUsageBucket(s, &objects.RCUsageBucket{
			BucketSeqno: seqno,
			Created:     created,
		})
	}

	// 4. rc_stats — RC_PENDING_STATS_ID + RC_ARCHIVE_STATS_ID. Empty
	// rows initialized so finalizeRCStats's promote-pending→archive
	// path finds an addressable target on its first crossing.
	if !objects.HasRCStats(s, objects.RCStatsIDPending) {
		objects.PutRCStats(s, objects.RCStatsIDPending, &objects.RCStats{})
	}
	if !objects.HasRCStats(s, objects.RCStatsIDArchive) {
		objects.PutRCStats(s, objects.RCStatsIDArchive, &objects.RCStats{})
	}

	// 5. WitnessSchedule.AccountSubsidyRd / AccountSubsidyWitnessRd.
	// Stored as scalar MedianAccountSubsidyBudget + Decay in the
	// gopherd schedule; if they're still at default 0 after this HF20
	// hook, seed from the resource_new_accounts canonical params so
	// account_subsidies has a non-zero pool to draw from until the
	// witness-median update kicks in.
	if ws, err := objects.GetWitnessSchedule(s); err == nil {
		if ws.MedianAccountSubsidyBudget == 0 {
			ws.MedianAccountSubsidyBudget = int32(rp.Entries[objects.RCNewAccounts].Dynamics.BudgetPerTime)
		}
		if ws.MedianAccountSubsidyDecay == 0 {
			ws.MedianAccountSubsidyDecay = rp.Entries[objects.RCNewAccounts].Dynamics.DecayPerTime
		}
		objects.PutWitnessSchedule(s, &ws)
	}
}
