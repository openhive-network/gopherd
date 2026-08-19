package chain

import (
	"math/big"

	"gopherd/internal/protocol/network"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// RC pricing: converting raw resource counts into RC cost.
//
// gopherd used to charge the raw resource count (execution nanoseconds +
// state bytes) directly as the RC cost, skipping hived's price curve
// entirely. The curve is what maps resource units onto the RC scale, and it
// makes hived's costs roughly two to three ORDERS OF MAGNITUDE smaller:
// measured on a loaded testnet, the same account sat at current_mana
// 3,641,947,366 on hived and -141,083,244 on gopherd.
//
// The visible consequence was that gopherd's producers built near-empty
// blocks — every candidate was refused with insufficient-rc while the hived
// witnesses packed the same transactions in (2.0 and 0.6 transactions per
// block against 13.9-41.5). RC is non-consensus (hived says so in
// rc_utility.cpp), which is why block IDs still matched throughout and only a
// load test could expose it.
//
// Mirrors resource_credits::compute_cost, rc_utility.cpp:257-330.

// rcRegenBlocks is HIVE_RC_REGEN_TIME / HIVE_BLOCK_INTERVAL — the number of
// blocks over which the whole vesting supply's RC regenerates.
const rcRegenBlocks = int64(objects.RCRegenTimeSec) / network.BlockIntervalSec

// computeCostOfResource is a direct port of
// resource_credits::compute_cost(curve_params, current_pool, resource_count,
// rc_regen).
//
//	num   = ((rc_regen * coeff_a) >> shift) + 1) * resource_count
//	denom = coeff_b + max(current_pool, 0)
//	cost  = num/denom + 1
//
// The intermediate overflows int64 well before realistic chain state (hived
// uses uint128 for exactly this reason), so the arithmetic runs through
// math/big.
func computeCostOfResource(curve objects.PriceCurve, currentPool, resourceCount, rcRegen int64) int64 {
	if rcRegen <= 0 {
		return 0
	}
	if resourceCount <= 0 {
		if resourceCount < 0 {
			return -computeCostOfResource(curve, currentPool, -resourceCount, rcRegen)
		}
		return 0
	}
	num := new(big.Int).SetInt64(rcRegen)
	num.Mul(num, new(big.Int).SetUint64(curve.CoeffA))
	// hived shifts BEFORE multiplying by resource_count: rc_regen * coeff_a
	// already risks overflowing 128 bits on its own.
	num.Rsh(num, uint(curve.Shift))
	// "err on the side of rounding not in the user's favor"
	num.Add(num, big.NewInt(1))
	num.Mul(num, new(big.Int).SetInt64(resourceCount))

	denom := new(big.Int).SetUint64(curve.CoeffB)
	// A negative pool does not push the price beyond p_max: p(x) = p(0) for x<0.
	if currentPool > 0 {
		denom.Add(denom, new(big.Int).SetInt64(currentPool))
	}
	if denom.Sign() == 0 {
		return 0
	}
	out := new(big.Int).Quo(num, denom)
	if !out.IsInt64() {
		return 0
	}
	return out.Int64() + 1
}

// rcCostOfUsage prices a transaction's per-resource usage against the current
// pools, mirroring resource_credits::compute_cost(rc_transaction_info*).
//
// Returns 0 when the RC subsystem is not initialised or total vesting is zero
// — hived's "when rc_regen is 0, everything is free".
func rcCostOfUsage(s *state.Session, usage [objects.RCNumResources]int64) int64 {
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return 0
	}
	rcRegen := dgpo.TotalVestingShares / rcRegenBlocks
	if rcRegen <= 0 {
		return 0
	}
	params, err := objects.GetRCResourceParam(s)
	if err != nil {
		return 0
	}
	pool, err := objects.GetRCPool(s)
	if err != nil {
		return 0
	}
	divisor := pool.SumOfResourceWeights
	if divisor == 0 {
		return 0
	}

	var total int64
	for i := 0; i < objects.RCNumResources; i++ {
		entry := params.Entries[i]
		// Usage is scaled by resource_unit BEFORE costing.
		scaled := usage[i] * int64(entry.Dynamics.ResourceUnit)

		share := new(big.Int).SetInt64(rcRegen)
		share.Mul(share, new(big.Int).SetUint64(uint64(pool.ResourceWeights[i])))
		share.Quo(share, new(big.Int).SetUint64(divisor))
		if !share.IsInt64() {
			continue
		}
		regenShare := share.Int64()
		if regenShare <= 0 {
			continue
		}
		total += computeCostOfResource(entry.Curve, pool.PoolArray[i], scaled, regenShare)
	}
	return total
}

// rcCostOfUsageDetailed is rcCostOfUsage but also returns the per-resource
// split, which rc_pending_data accumulates.
func rcCostOfUsageDetailed(s *state.Session, usage [objects.RCNumResources]int64) (int64, [objects.RCNumResources]int64) {
	var per [objects.RCNumResources]int64
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return 0, per
	}
	rcRegen := dgpo.TotalVestingShares / rcRegenBlocks
	if rcRegen <= 0 {
		return 0, per
	}
	params, err := objects.GetRCResourceParam(s)
	if err != nil {
		return 0, per
	}
	pool, err := objects.GetRCPool(s)
	if err != nil {
		return 0, per
	}
	divisor := pool.SumOfResourceWeights
	if divisor == 0 {
		return 0, per
	}
	var total int64
	for i := 0; i < objects.RCNumResources; i++ {
		entry := params.Entries[i]
		scaled := usage[i] * int64(entry.Dynamics.ResourceUnit)
		share := new(big.Int).SetInt64(rcRegen)
		share.Mul(share, new(big.Int).SetUint64(uint64(pool.ResourceWeights[i])))
		share.Quo(share, new(big.Int).SetUint64(divisor))
		if !share.IsInt64() {
			continue
		}
		regenShare := share.Int64()
		if regenShare <= 0 {
			continue
		}
		per[i] = computeCostOfResource(entry.Curve, pool.PoolArray[i], scaled, regenShare)
		total += per[i]
	}
	return total, per
}

// rcInelasticityThreshold is hive::chain::rc_curve_gen_params' default
// (rc_curve.hpp:11-18): coeff_b = pool_eq * 1/128.
//
// The a-point and u-point are NOT constants — resource_parameters.json carries
// them PER RESOURCE (state_bytes uses 150/10000, new_accounts 37/1000).
// Hardcoding new_accounts' 37/1000 for every resource priced state_bytes
// 0.037/0.015 = 2.467x too high, which is precisely the factor gopherd's
// effective curve was off by against a live hived node.
const (
	rcInelasticityThresholdNum   = 1
	rcInelasticityThresholdDenom = 128
)

// generateRCCurveParams ports hive::chain::generate_rc_curve_params
// (rc_curve.cpp:33-99).
//
//	A_num   = a_num * (u_denom - u_num)
//	A_denom = a_denom * u_num * decay_per_time_unit
//	shift   = shift1 + shift2, each normalising the leading bit to bit 63
//	coeff_a = (A_num << shift) / A_denom          (128-bit intermediate)
//	shift   = shift - decay_per_time_unit_denom_shift
//	coeff_b = pool_eq * inelasticity_num / inelasticity_denom
//
// The two-stage shift is what makes coeff_a a full-range uint64; computing it
// in one step underflows to a tiny value and prices everything wrongly.
func generateRCCurveParams(d *objects.ResourceDynamics, aPointNum, aPointDenom, uPointNum, uPointDenom uint64) objects.PriceCurve {
	var out objects.PriceCurve
	if d.DecayPerTime == 0 || uPointNum == 0 || aPointDenom == 0 {
		return out
	}
	aNum := aPointNum * (uPointDenom - uPointNum)
	aDenom := aPointDenom * uPointNum * uint64(d.DecayPerTime)
	if aNum < 1 {
		aNum = 1
	}
	if aDenom < 1 {
		aDenom = 1
	}
	shift1 := 63 - msb64(aNum)
	a1 := (aNum << shift1) / aDenom
	if a1 == 0 {
		return out
	}
	shift2 := 63 - msb64(a1)
	shift := shift1 + shift2
	if shift < d.DecayDenomShift {
		// hived asserts this; a curve generated below the decay shift would
		// produce a negative right-shift.
		return out
	}
	u128 := new(big.Int).SetUint64(aNum)
	u128.Lsh(u128, uint(shift))
	u128.Quo(u128, new(big.Int).SetUint64(aDenom))
	out.CoeffA = u128.Uint64()
	out.Shift = shift - d.DecayDenomShift

	b := new(big.Int).SetUint64(d.PoolEq)
	b.Mul(b, big.NewInt(rcInelasticityThresholdNum))
	b.Quo(b, big.NewInt(rcInelasticityThresholdDenom))
	out.CoeffB = b.Uint64()
	return out
}

// msb64 returns the index of the most significant set bit (0-based).
func msb64(v uint64) uint8 {
	n := uint8(0)
	for v > 1 {
		v >>= 1
		n++
	}
	return n
}
