package math128

import (
	"math/big"
	"math/rand"
	"testing"
)

func u128ToBig(u U128) *big.Int {
	b := new(big.Int).SetUint64(u.Hi)
	b.Lsh(b, 64)
	b.Or(b, new(big.Int).SetUint64(u.Lo))
	return b
}

func u192ToBig(u U192) *big.Int {
	b := new(big.Int).SetUint64(u.Hi)
	b.Lsh(b, 64)
	b.Or(b, new(big.Int).SetUint64(u.Mid))
	b.Lsh(b, 64)
	b.Or(b, new(big.Int).SetUint64(u.Lo))
	return b
}

func TestU128AddSub(t *testing.T) {
	for i := 0; i < 1000; i++ {
		a := U128{Hi: rand.Uint64(), Lo: rand.Uint64()}
		b := U128{Hi: rand.Uint64(), Lo: rand.Uint64()}
		sum := a.Add(b)
		want := new(big.Int).Add(u128ToBig(a), u128ToBig(b))
		// mod 2^128
		mask := new(big.Int).Lsh(big.NewInt(1), 128)
		want.Mod(want, mask)
		if u128ToBig(sum).Cmp(want) != 0 {
			t.Fatalf("Add: got %v want %v", u128ToBig(sum), want)
		}
		diff := a.Sub(b)
		want2 := new(big.Int).Sub(u128ToBig(a), u128ToBig(b))
		want2.Mod(want2, mask)
		// Mod of negative result in Go's big is non-negative — match wrap.
		if u128ToBig(diff).Cmp(want2) != 0 {
			t.Fatalf("Sub: got %v want %v", u128ToBig(diff), want2)
		}
	}
}

func TestMul64Wide(t *testing.T) {
	for i := 0; i < 1000; i++ {
		a := rand.Uint64()
		b := rand.Uint64()
		got := Mul64Wide(a, b)
		want := new(big.Int).Mul(new(big.Int).SetUint64(a), new(big.Int).SetUint64(b))
		if u128ToBig(got).Cmp(want) != 0 {
			t.Fatalf("Mul64Wide(%d, %d): got %v want %v", a, b, u128ToBig(got), want)
		}
	}
}

func TestMul64x128(t *testing.T) {
	for i := 0; i < 1000; i++ {
		a := rand.Uint64()
		b := U128{Hi: rand.Uint64(), Lo: rand.Uint64()}
		got := Mul64x128(a, b)
		want := new(big.Int).Mul(new(big.Int).SetUint64(a), u128ToBig(b))
		if u192ToBig(got).Cmp(want) != 0 {
			t.Fatalf("Mul64x128(%d, %v): got %v want %v", a, b, u192ToBig(got), want)
		}
	}
}

func TestU192QuoRem(t *testing.T) {
	// Cases where quotient fits in uint64.
	for i := 0; i < 2000; i++ {
		// Generate a uint64 quotient and a U128 divisor; n = q*d + r.
		q := rand.Uint64()
		d := U128{Hi: rand.Uint64() & 0xffff, Lo: rand.Uint64()}
		if d.IsZero() {
			d = U128{Lo: 1}
		}
		// r < d. Pick a random r below d.
		r := U128{Hi: rand.Uint64(), Lo: rand.Uint64()}
		for r.Cmp(d) >= 0 {
			r = U128{Hi: r.Hi >> 1, Lo: r.Lo}
		}
		// n = q*d + r as U192
		n := Mul64x128(q, d)
		// add r
		lo, c := add64(n.Lo, r.Lo, 0)
		mid, c2 := add64(n.Mid, r.Hi, c)
		hi, _ := add64(n.Hi, 0, c2)
		n2 := U192{Hi: hi, Mid: mid, Lo: lo}
		gq, gr := n2.QuoRem(d)
		if gq != q {
			t.Fatalf("QuoRem quot %d=%d*%v+%v: got q=%d want %d", i, q, d, r, gq, q)
		}
		if gr.Cmp(r) != 0 {
			t.Fatalf("QuoRem rem %d=%d*%v+%v: got r=%v want %v", i, q, d, r, gr, r)
		}
	}
}

// TestU128String ports basic_tests.cpp::fc_uint128_to_string. Walks a
// shifting bit pattern across all 128 positions, then a multiplicative
// growth from 333, then 1000 random Hi:Lo pairs. Compares against
// math/big as the canonical big-integer reference.
func TestU128String(t *testing.T) {
	// Shift case: start at 0xf, shift left 1 bit per step, 128 steps.
	fci := U128{Lo: 0xf}
	ref := new(big.Int).SetUint64(0xf)
	mask := new(big.Int).Lsh(big.NewInt(1), 128)
	for i := 1; i < 129; i++ {
		if fci.String() != ref.String() {
			t.Fatalf("shift i=%d: got %s want %s", i, fci.String(), ref.String())
		}
		// shift both left by 1
		fci = fci.Lsh1()
		ref.Lsh(ref, 1)
		ref.Mod(ref, mask)
	}

	// Growth case: start at 333, repeat += 261; *= i.
	fci = U128{Lo: 333}
	ref = new(big.Int).SetUint64(333)
	for i := 1; i < 1001; i++ {
		if fci.String() != ref.String() {
			t.Fatalf("growth i=%d: got %s want %s", i, fci.String(), ref.String())
		}
		fci = fci.Add64(261).Mul64(uint64(i))
		ref.Add(ref, big.NewInt(261)).Mul(ref, big.NewInt(int64(i))).Mod(ref, mask)
	}

	// Random case: 1000 random Hi:Lo pairs.
	for i := 0; i < 1000; i++ {
		hi := rand.Uint64()
		lo := rand.Uint64()
		v := FromHiLo(hi, lo)
		want := new(big.Int).SetUint64(hi)
		want.Lsh(want, 64)
		want.Or(want, new(big.Int).SetUint64(lo))
		if v.String() != want.String() {
			t.Fatalf("rand hi=%d lo=%d: got %s want %s", hi, lo, v.String(), want.String())
		}
	}
}

// TestU128ApproxSqrt covers the curation-weight formula in
// basic_tests.cpp::curation_weight_test. The first BOOST_REQUIRE checks
// uint128_approx_sqrt(2 * 2e12) == 2002250.
func TestU128ApproxSqrt(t *testing.T) {
	// First C++ case: rshares = 856158, s = 2_000_000_000_000.
	// approx_sqrt(rshares + 2s) == 2002250.
	rshares := uint64(856158)
	s := uint64(2_000_000_000_000)
	in := U128{Lo: rshares + 2*s}
	got := in.ApproxSqrt()
	if got != 2002250 {
		t.Fatalf("approx_sqrt(rshares+2s) with rshares=856158: got %d want 2002250", got)
	}

	// rshares = 0 → same sqrt result.
	in = U128{Lo: 2 * s}
	got = in.ApproxSqrt()
	if got != 2002250 {
		t.Fatalf("approx_sqrt(2s): got %d want 2002250", got)
	}

	// Sub of two equal U128 → 0.
	if !FromHiLo(0, 0).Sub(FromHiLo(0, 0)).IsZero() {
		t.Fatalf("0 - 0 != 0")
	}
}

func add64(a, b, c uint64) (uint64, uint64) {
	s := a + b + c
	carry := uint64(0)
	if s < a || (s == a && (b|c) != 0) {
		carry = 1
	}
	return s, carry
}
