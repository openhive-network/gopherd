package chain

import (
	"math"
	"math/big"
	"math/rand"
	"testing"

	"gopherd/internal/state/objects"
)

// computeCostOfResourceBig is the math/big implementation computeCostOfResource
// replaced, kept here as the reference the 128-bit version must agree with.
func computeCostOfResourceBig(curve objects.PriceCurve, currentPool, resourceCount, rcRegen int64) int64 {
	if rcRegen <= 0 {
		return 0
	}
	if resourceCount <= 0 {
		if resourceCount < 0 {
			return -computeCostOfResourceBig(curve, currentPool, -resourceCount, rcRegen)
		}
		return 0
	}
	num := new(big.Int).SetInt64(rcRegen)
	num.Mul(num, new(big.Int).SetUint64(curve.CoeffA))
	num.Rsh(num, uint(curve.Shift))
	num.Add(num, big.NewInt(1))
	num.Mul(num, new(big.Int).SetInt64(resourceCount))
	denom := new(big.Int).SetUint64(curve.CoeffB)
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

// TestComputeCostOfResourceMatchesBigInt pins the 128-bit port of hived's
// compute_cost against the exact big.Int arithmetic over mainnet-like and
// adversarial inputs, including the saturating cases.
func TestComputeCostOfResourceMatchesBigInt(t *testing.T) {
	cases := []struct {
		curve              objects.PriceCurve
		pool, count, regen int64
	}{
		{objects.PriceCurve{CoeffA: 1 << 40, CoeffB: 1 << 30, Shift: 20}, 1 << 40, 1000, 3_500_000_000_000},
		{objects.PriceCurve{CoeffA: math.MaxUint64, CoeffB: 1, Shift: 0}, 0, 1, 1},
		{objects.PriceCurve{CoeffA: math.MaxUint64, CoeffB: 1, Shift: 0}, -5, math.MaxInt64, math.MaxInt64},
		{objects.PriceCurve{CoeffA: 123456789, CoeffB: 0, Shift: 3}, 0, 10, 10},
		{objects.PriceCurve{CoeffA: 1, CoeffB: math.MaxUint64, Shift: 0}, math.MaxInt64, 7, 7},
		{objects.PriceCurve{CoeffA: 1 << 63, CoeffB: 1 << 10, Shift: 127}, 1, 1, 1 << 62},
		{objects.PriceCurve{CoeffA: 1 << 63, CoeffB: 1 << 10, Shift: 64}, 1 << 50, 1 << 20, 1 << 62},
	}
	r := rand.New(rand.NewSource(29))
	for i := 0; i < 20000; i++ {
		cases = append(cases, struct {
			curve              objects.PriceCurve
			pool, count, regen int64
		}{
			objects.PriceCurve{CoeffA: r.Uint64() >> uint(r.Intn(64)), CoeffB: r.Uint64() >> uint(r.Intn(64)), Shift: uint8(r.Intn(128))},
			int64(r.Uint64()>>uint(r.Intn(64))) - int64(r.Intn(2))*1000,
			int64(r.Uint64() >> uint(1+r.Intn(63))),
			int64(r.Uint64() >> uint(1+r.Intn(63))),
		})
	}
	for i, c := range cases {
		want := computeCostOfResourceBig(c.curve, c.pool, c.count, c.regen)
		got := computeCostOfResource(c.curve, c.pool, c.count, c.regen)
		if got != want {
			t.Fatalf("case %d %+v pool=%d count=%d regen=%d: got %d want %d", i, c.curve, c.pool, c.count, c.regen, got, want)
		}
	}
}

// TestRCRegenShareMatchesBigInt pins the per-resource regeneration share.
func TestRCRegenShareMatchesBigInt(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 20000; i++ {
		regen := int64(r.Uint64() >> uint(1+r.Intn(63)))
		weight := r.Uint64() >> uint(r.Intn(64))
		divisor := 1 + r.Uint64()>>uint(r.Intn(64))
		ref := new(big.Int).SetInt64(regen)
		ref.Mul(ref, new(big.Int).SetUint64(weight))
		ref.Quo(ref, new(big.Int).SetUint64(divisor))
		got, ok := rcRegenShare(regen, weight, divisor)
		if ok != ref.IsInt64() || (ok && got != ref.Int64()) {
			t.Fatalf("regen=%d weight=%d divisor=%d: got %d,%v want %s", regen, weight, divisor, got, ok, ref)
		}
	}
}
