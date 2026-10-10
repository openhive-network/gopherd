package chain

import (
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"os"
	"sync/atomic"

	"gopherd/internal/math128"
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
	// hived's uint128 arithmetic, done with math/bits: this runs once per
	// resource per transaction and the math/big version was 18% of all bytes
	// allocated during a mainnet sync. A quotient that does not fit int64
	// prices at 0, as before (hived would assert).
	num := math128.Mul64Wide(uint64(rcRegen), curve.CoeffA)
	num = u128Rsh(num, uint(curve.Shift))
	// "err on the side of rounding not in the user's favor"
	num = num.Add64(1)
	// hived shifts BEFORE multiplying by resource_count: rc_regen * coeff_a
	// already risks overflowing 128 bits on its own.
	num, overflow := u128Mul64Checked(num, uint64(resourceCount))
	if overflow {
		return 0
	}
	denomLo, carry := bits.Add64(curve.CoeffB, 0, 0)
	// A negative pool does not push the price beyond p_max: p(x) = p(0) for x<0.
	if currentPool > 0 {
		denomLo, carry = bits.Add64(curve.CoeffB, uint64(currentPool), 0)
	}
	denom := math128.U128{Hi: carry, Lo: denomLo}
	if denom.IsZero() {
		return 0
	}
	q, _ := math128.U192{Mid: num.Hi, Lo: num.Lo}.QuoRem(denom)
	if q > math.MaxInt64-1 {
		return 0 // saturated or not representable; the big.Int version returned 0 here too
	}
	return int64(q) + 1
}

// u128Rsh shifts a right by s bits (s < 128).
func u128Rsh(a math128.U128, s uint) math128.U128 {
	switch {
	case s == 0:
		return a
	case s >= 128:
		return math128.U128{}
	case s >= 64:
		return math128.U128{Lo: a.Hi >> (s - 64)}
	default:
		return math128.U128{Hi: a.Hi >> s, Lo: a.Lo>>s | a.Hi<<(64-s)}
	}
}

// u128Mul64Checked multiplies and reports whether the product exceeded 128 bits.
func u128Mul64Checked(a math128.U128, b uint64) (math128.U128, bool) {
	lhi, llo := bits.Mul64(a.Lo, b)
	hhi, hlo := bits.Mul64(a.Hi, b)
	hi, c := bits.Add64(lhi, hlo, 0)
	return math128.U128{Hi: hi, Lo: llo}, hhi != 0 || c != 0
}

// rcRegenShare is hived's `uint128_to_int64((uint128(rc_regen) * weight) /
// divisor)`: this resource's slice of the regeneration. ok is false when the
// result does not fit int64 (hived asserts; the caller skips the resource).
func rcRegenShare(rcRegen int64, weight, divisor uint64) (int64, bool) {
	q := math128.Mul64Wide(uint64(rcRegen), weight).DivU64(divisor)
	if q.Hi != 0 || q.Lo > math.MaxInt64 {
		return 0, false
	}
	return int64(q.Lo), true
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

		regenShare, ok := rcRegenShare(rcRegen, uint64(pool.ResourceWeights[i]), divisor)
		if !ok {
			continue
		}
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
		regenShare, ok := rcRegenShare(rcRegen, uint64(pool.ResourceWeights[i]), divisor)
		if !ok || regenShare <= 0 {
			continue
		}
		per[i] = computeCostOfResource(entry.Curve, pool.PoolArray[i], scaled, regenShare)
		total += per[i]
	}
	// GOPHERD_RC_DEBUG prints the per-resource breakdown for the first few
	// priced transactions. Kept because RC parity work is impossible without
	// it: an aggregate mana delta cannot distinguish a wrong PRICE from a
	// wrong USAGE from a wrong POOL, and every RC bug so far looked identical
	// in the aggregate.
	if os.Getenv("GOPHERD_RC_DEBUG") != "" && rcDebugCount.Add(1) <= 8 {
		fmt.Fprintf(os.Stderr, "RC DEBUG rc_regen=%d divisor=%d\n", rcRegen, divisor)
		for i := 0; i < objects.RCNumResources; i++ {
			e := params.Entries[i]
			fmt.Fprintf(os.Stderr, "  res%d usage=%-12d unit=%-6d pool=%-16d cost=%d\n",
				i, usage[i], e.Dynamics.ResourceUnit, pool.PoolArray[i], per[i])
		}
		fmt.Fprintf(os.Stderr, "  TOTAL cost=%d\n", total)
	}
	return total, per
}

var rcDebugCount atomic.Int64

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
