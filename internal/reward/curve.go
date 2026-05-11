// Package reward holds shared helpers for the comment cashout pipeline:
// the reward-curve formula and the uint128 (de)serializer used by
// dynamic_global_property_object.total_reward_shares2.
package reward

import (
	"math/bits"

	"hived-go/internal/math128"
)

// ContentConstantHF0Val — the `s` constant used by the quadratic reward
// curve. From libraries/protocol/include/hive/protocol/config.hpp:
//
//	#define HIVE_CONTENT_CONSTANT_HF0  (uint128_t(2000000000000ll))
const ContentConstantHF0Val uint64 = 2_000_000_000_000

// EvaluateRewardCurveQuadratic computes the C++ `quadratic` curve from
// libraries/chain/util/reward.cpp:34:
//
//	result = (rshares + content_constant)² - content_constant²
//	       = rshares² + 2·rshares·content_constant
//
// rshares can be up to ~1e15 in practice; rshares² needs uint128. We
// return a U128 directly. Negative rshares clamps to 0 to match the
// math/big path's `Sign() <= 0 ⇒ new(big.Int)`.
//
// Native math/bits replaces a math/big triple-alloc per call (was
// 634 MB cum / 4M-block run via adjustRewardShares2).
func EvaluateRewardCurveQuadratic(rshares int64) math128.U128 {
	if rshares <= 0 {
		return math128.U128{}
	}
	return curveQuadratic(uint64(rshares), ContentConstantHF0Val)
}

// curveQuadratic computes r² + 2·r·s as U128. Both factors fit in
// uint128 for any valid (r, s) on Hive — r is bounded by int64 (≤ 2^63),
// s is the content constant (~2^41) so r² ≤ 2^126 and 2·r·s ≤ 2^105.
func curveQuadratic(r, s uint64) math128.U128 {
	r2 := math128.Mul64Wide(r, r)
	// 2·r·s. r ≤ 2^63 ensures 2·r doesn't overflow uint64.
	rs := math128.Mul64Wide(r, s).Lsh1()
	return r2.Add(rs)
}

// Uint128FromBytes deserializes the 16-byte big-endian uint128 stored
// in DGPO.TotalRewardShares2.
func Uint128FromBytes(b [16]byte) math128.U128 {
	return math128.FromBytes16(b)
}

// Uint128ToBytes serializes v as a 16-byte big-endian uint128. Native
// U128 wraps mod 2^128 by construction, matching C++ fc::uint128_t.
func Uint128ToBytes(v math128.U128) [16]byte {
	return v.ToBytes16()
}

// silence unused import when math/bits isn't otherwise referenced.
var _ = bits.LeadingZeros64
