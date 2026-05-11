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

func add64(a, b, c uint64) (uint64, uint64) {
	s := a + b + c
	carry := uint64(0)
	if s < a || (s == a && (b|c) != 0) {
		carry = 1
	}
	return s, carry
}
