package objects

import (
	"math/big"
	"math/bits"
)

// Uint128 is a 128-bit unsigned integer stored as two uint64 (high + low),
// laid out so that Hi << 64 | Lo equals the value. We use it instead of
// math/big.Int in the witness-vote / virtual-time hot path to avoid the
// per-operation heap allocation that big.Int incurs (each AdjustWitnessVote
// call previously allocated 7+ big.Ints; over 30 witness updates per
// producer-reward block the GC pressure cost ~10% of replay CPU).
//
// Operations wrap modulo 2^128 — matching C++'s native uint128_t semantics
// and the chain's existing reliance on overflow wrap in HF4+ virtual_time
// math (see witness_schedule.cpp:344-349). Callers that need to detect
// wrap (e.g., the HF4 reset_virtual_schedule_time trigger) compare result
// vs. operand explicitly.
type Uint128 struct {
	Hi, Lo uint64
}

// Add returns a + b mod 2^128.
func (a Uint128) Add(b Uint128) Uint128 {
	lo, c := bits.Add64(a.Lo, b.Lo, 0)
	hi, _ := bits.Add64(a.Hi, b.Hi, c)
	return Uint128{Hi: hi, Lo: lo}
}

// Add64 returns a + b mod 2^128, where b is uint64.
func (a Uint128) Add64(b uint64) Uint128 {
	lo, c := bits.Add64(a.Lo, b, 0)
	hi, _ := bits.Add64(a.Hi, 0, c)
	return Uint128{Hi: hi, Lo: lo}
}

// Sub returns a - b mod 2^128 (wraps on borrow).
func (a Uint128) Sub(b Uint128) Uint128 {
	lo, br := bits.Sub64(a.Lo, b.Lo, 0)
	hi, _ := bits.Sub64(a.Hi, b.Hi, br)
	return Uint128{Hi: hi, Lo: lo}
}

// Mul64 returns a × b mod 2^128, where b is uint64. Useful for the
// `votes × elapsed` accumulator step.
func (a Uint128) Mul64(b uint64) Uint128 {
	hi1, lo := bits.Mul64(a.Lo, b)
	hi2 := a.Hi * b
	return Uint128{Hi: hi1 + hi2, Lo: lo}
}

// Cmp returns -1 if a < b, 0 if a == b, 1 if a > b.
func (a Uint128) Cmp(b Uint128) int {
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

// Quo64 returns a / b for b ≤ 2^64-1. Implements long division using
// math/bits.Div64. Panics on b == 0.
//
// bits.Div64(hi, lo, y) returns (quo, rem) — the QUOTIENT first, the
// remainder second. An earlier version of this routine swapped the
// returns (`_, lo := bits.Div64(...)`), making AdjustWitnessVote produce
// the per-vote *remainder* instead of the rolling-time advance. That
// poisoned every witness's virtual_scheduled_time, drifted the median
// price feed, and broke strict replay at block 2,927,795 with a 1-sat
// HBD short on linouxis9. Always assign the FIRST return value.
func (a Uint128) Quo64(b uint64) Uint128 {
	if b == 0 {
		panic("Uint128.Quo64: divide by zero")
	}
	if a.Hi < b {
		// a < b * 2^64, quotient fits in one uint64.
		quo, _ := bits.Div64(a.Hi, a.Lo, b)
		return Uint128{Hi: 0, Lo: quo}
	}
	// Long division: quotient of (0:a.Hi)/b, then carry remainder into
	// the low half.
	quoHi, rem := bits.Div64(0, a.Hi, b)
	quoLo, _ := bits.Div64(rem, a.Lo, b)
	return Uint128{Hi: quoHi, Lo: quoLo}
}

// Quo returns a / b. Panics on b == 0. Implements binary long division.
// Used by adjust_witness_vote when the divisor (votes+1) needs C++-style
// sign-extended uint128 semantics (negative int64 → uint128(max, ...) → huge
// divisor → ~0 quotient), which Quo64 cannot represent.
func (a Uint128) Quo(b Uint128) Uint128 {
	if b.Hi == 0 && b.Lo == 0 {
		panic("Uint128.Quo: divide by zero")
	}
	// Fast path: divisor fits in 64 bits.
	if b.Hi == 0 {
		return a.Quo64(b.Lo)
	}
	// Long division: shift divisor up until it exceeds dividend, then
	// shift down subtracting where possible.
	if a.Cmp(b) < 0 {
		return Uint128{}
	}
	// Find shift n such that (b << n) <= a < (b << (n+1)).
	d := b
	n := 0
	for d.Cmp(a) <= 0 && d.Hi&(1<<63) == 0 {
		d = Uint128{Hi: d.Hi<<1 | d.Lo>>63, Lo: d.Lo << 1}
		n++
	}
	q := Uint128{}
	for ; n >= 0; n-- {
		if a.Cmp(d) >= 0 {
			a = a.Sub(d)
			q = Uint128{Hi: q.Hi<<1 | q.Lo>>63, Lo: q.Lo<<1 | 1}
		} else {
			q = Uint128{Hi: q.Hi<<1 | q.Lo>>63, Lo: q.Lo << 1}
		}
		// Shift d right by 1.
		d = Uint128{Hi: d.Hi >> 1, Lo: d.Lo>>1 | d.Hi<<63}
	}
	return q
}

// Mod128 returns a mod m. The chain only uses m = 2^128 (free, since we
// already wrap) and m = LAP_LENGTH2 = 2^128 - 1. The 2^128 case is a
// no-op — return a unchanged. Other moduli aren't used.
//
// For LAP_LENGTH2 (max - 1), m = 2^128 - 1 = ^uint64(0)<<64 | ^uint64(0).
// Mod by max-1 is `if a == max then 0 else a` because a is already in
// [0, 2^128), so a mod (2^128-1) = 0 if a == 2^128-1 else a.
func (a Uint128) Mod128() Uint128 { return a }

// IsZero reports whether a is zero.
func (a Uint128) IsZero() bool { return a.Hi == 0 && a.Lo == 0 }

// PackBE16 returns the 16-byte big-endian encoding.
func (a Uint128) PackBE16() [16]byte {
	var out [16]byte
	out[0] = byte(a.Hi >> 56)
	out[1] = byte(a.Hi >> 48)
	out[2] = byte(a.Hi >> 40)
	out[3] = byte(a.Hi >> 32)
	out[4] = byte(a.Hi >> 24)
	out[5] = byte(a.Hi >> 16)
	out[6] = byte(a.Hi >> 8)
	out[7] = byte(a.Hi)
	out[8] = byte(a.Lo >> 56)
	out[9] = byte(a.Lo >> 48)
	out[10] = byte(a.Lo >> 40)
	out[11] = byte(a.Lo >> 32)
	out[12] = byte(a.Lo >> 24)
	out[13] = byte(a.Lo >> 16)
	out[14] = byte(a.Lo >> 8)
	out[15] = byte(a.Lo)
	return out
}

// UnpackUint128BE decodes 16 BE bytes.
func UnpackUint128BE(b [16]byte) Uint128 {
	hi := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	lo := uint64(b[8])<<56 | uint64(b[9])<<48 | uint64(b[10])<<40 | uint64(b[11])<<32 |
		uint64(b[12])<<24 | uint64(b[13])<<16 | uint64(b[14])<<8 | uint64(b[15])
	return Uint128{Hi: hi, Lo: lo}
}

// Uint128Max is 2^128 - 1, the LAP_LENGTH2 / lap128Max sentinel used by
// HF4+ overflow clamp.
var Uint128Max = Uint128{Hi: ^uint64(0), Lo: ^uint64(0)}

// Uint128MaxBig mirrors Uint128Max as a big.Int for legacy callers that
// still use math/big.
var Uint128MaxBig = new(big.Int).SetBytes([]byte{
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
})
