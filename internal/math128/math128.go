// Package math128 implements 128-bit and 192-bit unsigned integer math
// using only math/bits. Used by reward-curve / cashout / vesting paths
// that need wider-than-int64 precision but can't afford the per-op
// math/big heap allocation (which dominated GC pressure pre-conversion).
package math128

import (
	"encoding/binary"
	"math/bits"
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
