package chain

import (
	"testing"

	"gopherd/internal/state/objects"
)

// TestRCCurveParamsMatchHived pins gopherd's generated price curve against the
// values a live hived testnet reports through rc_api.get_resource_params.
//
// These coefficients set the RC PRICE. gopherd previously used a placeholder
// (coeff_a = pool_eq*a_num/a_denom, coeff_b = pool_eq*u_num/u_denom, shift = 0)
// that is not the generator at all, so it over-charged RC by orders of
// magnitude: every account hit the -10% mana floor and gopherd's witnesses
// produced near-empty blocks while hived's packed the same transactions in.
func TestRCCurveParamsMatchHived(t *testing.T) {
	// resource_state_bytes, taken from the running hived testnet.
	//   pool_eq  27139923979648  (coeff_b * 128)
	//   decay_per_time_unit / denom_shift from the same jsonball params
	for _, c := range []struct {
		name        string
		poolEq      uint64
		decay       uint32
		denomShift  uint8
		wantCoeffA  uint64
		wantCoeffB  uint64
		wantShiftLE uint8
	}{
		{
			name: "state_bytes", poolEq: 27139923979648,
			decay: 110339, denomShift: 36,
			wantCoeffB: 212030656091,
		},
	} {
		// state_bytes uses a=150/10000, u=1/2 (resource_parameters.json).
		got := generateRCCurveParams(&objects.ResourceDynamics{
			PoolEq: c.poolEq, DecayPerTime: c.decay, DecayDenomShift: c.denomShift,
		}, 150, 10000, 1, 2)
		if got.CoeffB != c.wantCoeffB {
			t.Errorf("%s coeff_b = %d, want %d (pool_eq * 1/128)", c.name, got.CoeffB, c.wantCoeffB)
		}
		// coeff_a must occupy the top of the uint64 range: the two-stage shift
		// exists precisely to normalise it there. A tiny value means the shift
		// normalisation was skipped, which is what the placeholder did.
		if got.CoeffA < 1<<62 {
			t.Errorf("%s coeff_a = %d, want a full-range uint64 (>= 2^62)", c.name, got.CoeffA)
		}
		if got.Shift == 0 {
			t.Errorf("%s shift = 0; the generator always produces a positive shift", c.name)
		}
		// The scale-invariant comparison: coeff_a/2^shift == (A_num/A_denom)*2^36.
		// hived reports coeff_a=10525659774662010880 shift=50 for state_bytes,
		// i.e. 9346.6. Comparing coeff_a alone is meaningless because the
		// generator is free to pick any (coeff_a, shift) pair on that ray.
		eff := float64(got.CoeffA) / float64(uint64(1)<<got.Shift)
		const hivedEff = 10525659774662010880.0 / (1 << 50)
		if ratio := eff / hivedEff; ratio < 0.99 || ratio > 1.01 {
			t.Errorf("%s effective A = %.1f, want %.1f (hived) — ratio %.3f",
				c.name, eff, hivedEff, ratio)
		}
		t.Logf("%s: coeff_a=%d coeff_b=%d shift=%d", c.name, got.CoeffA, got.CoeffB, got.Shift)
	}
}
