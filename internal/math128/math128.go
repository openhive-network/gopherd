// Package math128 implements 128-bit and 192-bit unsigned integer math
// using only math/bits. Used by reward-curve / cashout / vesting paths
// that need wider-than-int64 precision but can't afford the per-op
// math/big heap allocation (which dominated GC pressure pre-conversion).
package math128

import (
	"encoding/binary"
	"math/bits"
	"strconv"
)

// U128 is a 128-bit unsigned integer laid out so that Hi << 64 | Lo
// equals the value. Operations wrap modulo 2^128.
type U128 struct {
	Hi, Lo uint64
}

// From64 returns U128(v).
func From64(v uint64) U128 { return U128{Lo: v} }

// FromBytes16 decodes a 16-byte big-endian uint128.
func FromBytes16(b [16]byte) U128 {
	return U128{
		Hi: binary.BigEndian.Uint64(b[0:8]),
		Lo: binary.BigEndian.Uint64(b[8:16]),
	}
}

// ToBytes16 serializes a as a 16-byte big-endian uint128.
func (a U128) ToBytes16() [16]byte {
	var out [16]byte
	binary.BigEndian.PutUint64(out[0:8], a.Hi)
	binary.BigEndian.PutUint64(out[8:16], a.Lo)
	return out
}

// Add returns a + b mod 2^128.
func (a U128) Add(b U128) U128 {
	lo, c := bits.Add64(a.Lo, b.Lo, 0)
	hi, _ := bits.Add64(a.Hi, b.Hi, c)
	return U128{Hi: hi, Lo: lo}
}

// AddOverflow returns (a + b mod 2^128, carry) where carry=1 if the
// 128-bit sum wrapped.
func (a U128) AddOverflow(b U128) (U128, uint64) {
	lo, c := bits.Add64(a.Lo, b.Lo, 0)
	hi, c2 := bits.Add64(a.Hi, b.Hi, c)
	return U128{Hi: hi, Lo: lo}, c2
}

// DivU64 returns a / d as a U128 quotient. d must be non-zero (returns
// zero on d==0).
func (a U128) DivU64(d uint64) U128 {
	if d == 0 {
		return U128{}
	}
	if a.Hi == 0 {
		return U128{Lo: a.Lo / d}
	}
	qHi, rHi := a.Hi/d, a.Hi%d
	qLo, _ := bits.Div64(rHi, a.Lo, d)
	return U128{Hi: qHi, Lo: qLo}
}

// Sub returns a - b mod 2^128 (wraps on borrow — mirrors C++
// fc::uint128_t which is unsigned and silently wraps).
func (a U128) Sub(b U128) U128 {
	lo, br := bits.Sub64(a.Lo, b.Lo, 0)
	hi, _ := bits.Sub64(a.Hi, b.Hi, br)
	return U128{Hi: hi, Lo: lo}
}

// Add64 returns a + b mod 2^128 where b is uint64.
func (a U128) Add64(b uint64) U128 {
	lo, c := bits.Add64(a.Lo, b, 0)
	hi, _ := bits.Add64(a.Hi, 0, c)
	return U128{Hi: hi, Lo: lo}
}

// Mul64Wide returns a × b as a 128-bit value (a, b are uint64).
func Mul64Wide(a, b uint64) U128 {
	hi, lo := bits.Mul64(a, b)
	return U128{Hi: hi, Lo: lo}
}

// Lsh1 returns a << 1 mod 2^128 (multiply by 2).
func (a U128) Lsh1() U128 {
	hi := (a.Hi << 1) | (a.Lo >> 63)
	lo := a.Lo << 1
	return U128{Hi: hi, Lo: lo}
}

// IsZero reports a == 0.
func (a U128) IsZero() bool { return a.Hi == 0 && a.Lo == 0 }

// Cmp returns -1/0/+1 if a is less/equal/greater than b.
func (a U128) Cmp(b U128) int {
	if a.Hi != b.Hi {
		if a.Hi < b.Hi {
			return -1
		}
		return 1
	}
	if a.Lo != b.Lo {
		if a.Lo < b.Lo {
			return -1
		}
		return 1
	}
	return 0
}

// String returns a base-10 decimal representation of a, matching
// std::to_string(fc::uint128_t). Mirrors libraries/fc/src/uint128.cpp's
// chunk-by-1e19 algorithm so output matches the C++ canonical form.
func (a U128) String() string {
	if a.Hi == 0 {
		return strconv.FormatUint(a.Lo, 10)
	}
	const p10 uint64 = 10_000_000_000_000_000_000 // 10^19
	// Divide a by p10 → q (U128), r (uint64).
	hiQ, hiR := a.Hi/p10, a.Hi%p10
	loQ, loR := bits.Div64(hiR, a.Lo, p10)
	q := U128{Hi: hiQ, Lo: loQ}
	r := loR
	// Recurse on q (≤ 2^128 / 10^19 ≈ 3.4e19 — at most 2 chunks).
	prefix := q.String()
	suffix := strconv.FormatUint(r, 10)
	// Pad suffix to 19 digits.
	if pad := 19 - len(suffix); pad > 0 {
		suffix = "0000000000000000000"[:pad] + suffix
	}
	return prefix + suffix
}

// findMSB returns the bit index of the highest set bit in a (0..127).
// Returns 0 when a == 0 to mirror C++ uint128_find_msb's `x = 1` guard.
func (a U128) findMSB() uint8 {
	if a.Hi != 0 {
		return uint8(bits.Len64(a.Hi) - 1 + 64)
	}
	if a.Lo == 0 {
		return 0
	}
	return uint8(bits.Len64(a.Lo) - 1)
}

// ApproxSqrt returns an approximation of floor(sqrt(a)) using the same
// mantissa/exponent split as fc::uint128_approx_sqrt
// (libraries/fc/src/uint128.cpp:187). Used by the curation-weight
// formula (rshares / sqrt(rshares + 2s)).
func (a U128) ApproxSqrt() uint64 {
	if a.IsZero() {
		return 0
	}
	msbX := a.findMSB()
	msbZ := msbX >> 1
	// msb_x_bit = 1 << msb_x
	var msbXBit U128
	if msbX >= 64 {
		msbXBit = U128{Hi: 1 << (msbX - 64)}
	} else {
		msbXBit = U128{Lo: 1 << msbX}
	}
	msbZBit := uint64(1) << msbZ
	// mantissa_mask = msb_x_bit - 1
	mantissaMask := msbXBit.Sub(U128{Lo: 1})
	// mantissa_x = x & mantissa_mask
	mantissaX := U128{Hi: a.Hi & mantissaMask.Hi, Lo: a.Lo & mantissaMask.Lo}
	// mantissa_z_hi = (msb_x & 1) ? msb_z_bit : 0
	var mantissaZHi uint64
	if msbX&1 == 1 {
		mantissaZHi = msbZBit
	}
	// mantissa_z_lo = low_bits(mantissa_x >> (msb_x - msb_z))
	shift := msbX - msbZ
	var shifted U128
	if shift >= 128 {
		shifted = U128{}
	} else if shift >= 64 {
		shifted = U128{Lo: mantissaX.Hi >> (shift - 64)}
	} else if shift == 0 {
		shifted = mantissaX
	} else {
		shifted = U128{
			Hi: mantissaX.Hi >> shift,
			Lo: (mantissaX.Lo >> shift) | (mantissaX.Hi << (64 - shift)),
		}
	}
	mantissaZLo := shifted.Lo
	mantissaZ := (mantissaZHi | mantissaZLo) >> 1
	return msbZBit | mantissaZ
}

// To64 returns a.Lo (truncating to uint64). Matches fc::uint128_to_uint64.
func (a U128) To64() uint64 { return a.Lo }

// FromHiLo constructs a U128 from explicit hi/lo halves.
func FromHiLo(hi, lo uint64) U128 { return U128{Hi: hi, Lo: lo} }

// Mul64 returns a × b (b is uint64) as U128. May wrap if the true
// product exceeds 2^128 (caller's responsibility).
func (a U128) Mul64(b uint64) U128 {
	// (Hi:Lo) × b = (Hi × b) << 64 + Lo × b
	//             = hhi:hlo << 64 + lhi:llo
	//             = hhi << 128 + (hlo + lhi) << 64 + llo
	// mod 2^128: hi = hlo + lhi (with carry discarded), lo = llo.
	lhi, llo := bits.Mul64(a.Lo, b)
	_, hlo := bits.Mul64(a.Hi, b)
	hi := lhi + hlo
	return U128{Hi: hi, Lo: llo}
}

// U192 is a 192-bit unsigned integer (Hi << 128 | Mid << 64 | Lo).
// Used when uint64 × uint128 / uint128 needs intermediate uint192
// precision — specifically the cashout payout formula
// (fund_balance × weighted_curve / shares2).
type U192 struct {
	Hi, Mid, Lo uint64
}

// Mul64x128 returns a × b as a U192. a is uint64, b is U128.
func Mul64x128(a uint64, b U128) U192 {
	// a × (b.Hi << 64 + b.Lo) = (a × b.Hi) << 64 + a × b.Lo
	lhi, llo := bits.Mul64(a, b.Lo) // a × b.Lo = lhi:llo
	hhi, hlo := bits.Mul64(a, b.Hi) // a × b.Hi = hhi:hlo
	// Result is hhi:hlo:0 + 0:lhi:llo
	// = hhi : (hlo + lhi w/ carry) : llo
	midSum, c := bits.Add64(lhi, hlo, 0)
	hiOut, _ := bits.Add64(hhi, 0, c)
	return U192{Hi: hiOut, Mid: midSum, Lo: llo}
}

// QuoRem returns q, r such that a = q*d + r, where d is a U128 and the
// quotient q is assumed to fit in uint64. Returns ^uint64(0) (saturated)
// when the true quotient overflows uint64 — caller's responsibility to
// ensure the result fits.
//
// Implementation: normalized Knuth Algorithm D. Computes an initial
// quotient estimate from the top two words of n divided by the top word
// of d (after left-shifting both so d's high bit is set), then corrects
// the estimate by at most 2.
func (a U192) QuoRem(d U128) (q uint64, r U128) {
	if d.Hi == 0 && d.Lo == 0 {
		return 0, U128{}
	}
	// Fast path: d fits in uint64.
	if d.Hi == 0 {
		if a.Hi != 0 || a.Mid >= d.Lo {
			return ^uint64(0), U128{}
		}
		q, remLo := bits.Div64(a.Mid, a.Lo, d.Lo)
		return q, U128{Lo: remLo}
	}
	// d.Hi > 0. Normalize: shift both n and d left so d.Hi has its high bit set.
	shift := uint(bits.LeadingZeros64(d.Hi))
	dn := d
	an := a
	if shift > 0 {
		dn.Hi = (d.Hi << shift) | (d.Lo >> (64 - shift))
		dn.Lo = d.Lo << shift
		// q fits in uint64 ⇒ a < d * 2^64 ⇒ a < 2^192 (always) and the
		// top `shift` bits of a.Hi are zero, so the left-shift fits in U192.
		an.Hi = (a.Hi << shift) | (a.Mid >> (64 - shift))
		an.Mid = (a.Mid << shift) | (a.Lo >> (64 - shift))
		an.Lo = a.Lo << shift
	}

	// q_hat = floor((an.Hi:an.Mid) / dn.Hi), saturated to uint64-1 when
	// the true value would overflow.
	var qhat uint64
	if an.Hi == 0 {
		qhat = an.Mid / dn.Hi
	} else if an.Hi >= dn.Hi {
		qhat = ^uint64(0)
	} else {
		qhat, _ = bits.Div64(an.Hi, an.Mid, dn.Hi)
	}

	// Refine qhat downward (at most 2 corrections per Knuth).
	for k := 0; k < 3 && qhat > 0; k++ {
		prod := Mul64x128(qhat, dn)
		if prod.Hi < an.Hi ||
			(prod.Hi == an.Hi && prod.Mid < an.Mid) ||
			(prod.Hi == an.Hi && prod.Mid == an.Mid && prod.Lo <= an.Lo) {
			break
		}
		qhat--
	}

	// Compute remainder = an - qhat*dn (fits in U128 since qhat is the true quotient).
	prod := Mul64x128(qhat, dn)
	rLo, br := bits.Sub64(an.Lo, prod.Lo, 0)
	rMid, br := bits.Sub64(an.Mid, prod.Mid, br)
	_, _ = bits.Sub64(an.Hi, prod.Hi, br)

	// De-normalize the remainder.
	var rOut U128
	if shift == 0 {
		rOut = U128{Hi: rMid, Lo: rLo}
	} else {
		rOut.Lo = (rLo >> shift) | (rMid << (64 - shift))
		rOut.Hi = rMid >> shift
	}
	return qhat, rOut
}
