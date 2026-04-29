// Package reward holds shared helpers for the comment cashout pipeline:
// the reward-curve formula and the uint128 (de)serializer used by
// dynamic_global_property_object.total_reward_shares2.
package reward

import (
	"math/big"
)

// ContentConstantHF0 — the `s` constant used by the quadratic reward
// curve. From libraries/protocol/include/hive/protocol/config.hpp:
//
//	#define HIVE_CONTENT_CONSTANT_HF0  (uint128_t(2000000000000ll))
//
// Pre-HF17 hived's only curve is `quadratic` with this constant; after
// HF17 the curve is configurable per reward_fund. We use the pre-HF17
// curve unconditionally for now (the chain section currently under
// test is well below HF17 at block 10,629,455).
var ContentConstantHF0 = big.NewInt(2_000_000_000_000)

// EvaluateRewardCurveQuadratic computes the C++ `quadratic` curve from
// libraries/chain/util/reward.cpp:34:
//
//	result = (rshares + content_constant)² - content_constant²
//	       = rshares² + 2·rshares·content_constant
//
// rshares can be up to ~1e15 in practice, so rshares² overflows int64
// and the curve value definitely overflows uint64. We use math/big.Int.
func EvaluateRewardCurveQuadratic(rshares int64) *big.Int {
	if rshares <= 0 {
		return new(big.Int)
	}
	r := big.NewInt(rshares)
	rPlusS := new(big.Int).Add(r, ContentConstantHF0)
	out := new(big.Int).Mul(rPlusS, rPlusS)
	cc2 := new(big.Int).Mul(ContentConstantHF0, ContentConstantHF0)
	out.Sub(out, cc2)
	return out
}

// Uint128FromBytes deserializes the 16-byte big-endian uint128 stored
// in DGPO.TotalRewardShares2.
func Uint128FromBytes(b [16]byte) *big.Int {
	out := new(big.Int)
	out.SetBytes(b[:])
	return out
}

// Uint128ToBytes serializes a non-negative big.Int as 16-byte big-endian.
// Caller is responsible for ensuring v fits in 128 bits.
func Uint128ToBytes(v *big.Int) [16]byte {
	var out [16]byte
	if v.Sign() <= 0 {
		return out
	}
	src := v.Bytes()
	if len(src) > 16 {
		src = src[len(src)-16:]
	}
	copy(out[16-len(src):], src)
	return out
}
