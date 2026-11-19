package math128

import (
	"math/big"
	"testing"
)

var two128 = new(big.Int).Lsh(big.NewInt(1), 128)

func toBig(a U128) *big.Int {
	v := new(big.Int).SetUint64(a.Hi)
	v.Lsh(v, 64)
	return v.Or(v, new(big.Int).SetUint64(a.Lo))
}

func mod128(v *big.Int) *big.Int { return new(big.Int).Mod(v, two128) }

// FuzzU128MatchesBig checks every U128 operation against math/big: wrapping
// add and subtract, 64x64 and 128x64 multiply, division by a uint64, compare,
// the 16-byte encoding, and the 192/128 quotient used by reward curves. These
// carry reward shares and payouts; a carry bug is a silent fork.
func FuzzU128MatchesBig(f *testing.F) {
	f.Add(uint64(1), uint64(0), uint64(0), uint64(1), uint64(3))
	f.Add(^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0))
	f.Add(uint64(0), uint64(1<<63), uint64(1), uint64(0), uint64(1<<32))
	f.Fuzz(func(t *testing.T, ahi, alo, bhi, blo, k uint64) {
		a, b := FromHiLo(ahi, alo), FromHiLo(bhi, blo)
		A, B, K := toBig(a), toBig(b), new(big.Int).SetUint64(k)

		if got, want := toBig(a.Add(b)), mod128(new(big.Int).Add(A, B)); got.Cmp(want) != 0 {
			t.Fatalf("%v + %v = %v, want %v", A, B, got, want)
		}
		if got, want := toBig(a.Sub(b)), mod128(new(big.Int).Sub(A, B)); got.Cmp(want) != 0 {
			t.Fatalf("%v - %v = %v, want %v", A, B, got, want)
		}
		if got, want := toBig(a.Add64(k)), mod128(new(big.Int).Add(A, K)); got.Cmp(want) != 0 {
			t.Fatalf("%v + %d = %v, want %v", A, k, got, want)
		}
		if got, want := toBig(Mul64Wide(alo, k)), new(big.Int).Mul(new(big.Int).SetUint64(alo), K); got.Cmp(want) != 0 {
			t.Fatalf("%d * %d = %v, want %v", alo, k, got, want)
		}
		if got, want := toBig(a.Mul64(k)), mod128(new(big.Int).Mul(A, K)); got.Cmp(want) != 0 {
			t.Fatalf("%v * %d = %v, want %v", A, k, got, want)
		}
		if k != 0 {
			if got, want := toBig(a.DivU64(k)), new(big.Int).Quo(A, K); got.Cmp(want) != 0 {
				t.Fatalf("%v / %d = %v, want %v", A, k, got, want)
			}
		}
		if got, want := a.Cmp(b), A.Cmp(B); got != want {
			t.Fatalf("Cmp(%v, %v) = %d, want %d", A, B, got, want)
		}
		if FromBytes16(a.ToBytes16()) != a {
			t.Fatalf("16-byte encoding of %v does not round-trip", A)
		}
		if got, want := toBig(a.Lsh1()), mod128(new(big.Int).Lsh(A, 1)); got.Cmp(want) != 0 {
			t.Fatalf("%v << 1 = %v, want %v", A, got, want)
		}

		// 64x128 -> 192, then quotient and remainder by a nonzero U128
		// when the quotient fits in 64 bits.
		p := Mul64x128(k, b)
		P := new(big.Int).Mul(K, B)
		if !b.IsZero() {
			q, r := p.QuoRem(a)
			if !a.IsZero() {
				Q, R := new(big.Int).QuoRem(P, A, new(big.Int))
				if Q.IsUint64() {
					if q != Q.Uint64() || toBig(r).Cmp(R) != 0 {
						t.Fatalf("(%d*%v) / %v = (%d, %v), want (%v, %v)", k, B, A, q, toBig(r), Q, R)
					}
				}
			}
		}
	})
}

// FuzzApproxSqrt bounds U128.ApproxSqrt, fc::uint128_approx_sqrt's port: it
// is an approximation by design, but within a factor of 9/8 of the exact
// root and monotone, as fc's mantissa/exponent split guarantees.
func FuzzApproxSqrt(f *testing.F) {
	f.Add(uint64(0), uint64(1))
	f.Add(uint64(0), uint64(2))
	f.Add(^uint64(0), ^uint64(0))
	f.Add(uint64(1), uint64(0))
	f.Fuzz(func(t *testing.T, hi, lo uint64) {
		a := FromHiLo(hi, lo)
		got := a.ApproxSqrt()
		if a.IsZero() {
			if got != 0 {
				t.Fatalf("sqrt(0) = %d", got)
			}
			return
		}
		exact := new(big.Int).Sqrt(toBig(a)).Uint64()
		lo8, hi8 := new(big.Int).SetUint64(exact), new(big.Int).SetUint64(exact)
		lo8.Mul(lo8, big.NewInt(7)).Quo(lo8, big.NewInt(8))
		hi8.Mul(hi8, big.NewInt(9)).Quo(hi8, big.NewInt(8)).Add(hi8, big.NewInt(1))
		if g := new(big.Int).SetUint64(got); g.Cmp(lo8) < 0 || g.Cmp(hi8) > 0 {
			t.Fatalf("ApproxSqrt(%v) = %d, exact %d", toBig(a), got, exact)
		}
	})
}
