package objects

import (
	"math"
	"math/big"
	"testing"
)

// hivedRegenerateMana is util::manabar::regenerate_mana (chain/util/
// manabar.hpp:42-68) written out with math/big. ok is false where hived
// throws: now < last_update_time, or a regeneration that does not fit int64
// (uint128_to_uint64's assert, then the u_regen <= INT64_MAX assert).
func hivedRegenerateMana(current int64, last uint32, maxMana int64, regen uint32, now uint32, skipCap bool) (mana int64, update uint32, ok bool) {
	if now < last {
		return 0, 0, false
	}
	dt := now - last
	if current >= maxMana {
		return maxMana, now, true
	}
	if dt == 0 {
		return current, last, true
	}
	if !skipCap && dt > regen {
		dt = regen
	}
	m := big.NewInt(0)
	if maxMana > 0 {
		m.SetInt64(maxMana)
	}
	m.Mul(m, big.NewInt(int64(dt)))
	m.Quo(m, big.NewInt(int64(regen)))
	if !m.IsInt64() {
		return 0, 0, false
	}
	u := m.Int64()
	sum := new(big.Int).Add(big.NewInt(current), big.NewInt(u))
	var next int64
	switch {
	case sum.Cmp(big.NewInt(math.MaxInt64)) > 0:
		next = math.MaxInt64
	case sum.Cmp(big.NewInt(math.MinInt64)) < 0:
		next = math.MinInt64
	default:
		next = sum.Int64()
	}
	if next > maxMana {
		next = maxMana
	}
	return next, now, true
}

// FuzzRegenerateManaMatchesHived compares RegenerateMana with hived's
// regenerate_mana on every input hived accepts. The two must agree exactly:
// voting and RC mana feed vote weights and the RC stamps, so a one-unit
// difference is a fork.
func FuzzRegenerateManaMatchesHived(f *testing.F) {
	f.Add(int64(50), uint32(1000), int64(100), uint32(432000), uint32(87400), true)
	f.Add(int64(0), uint32(0), int64(100_000_000_000_000_000), uint32(432000), uint32(1_600_000_000), true)
	f.Add(int64(-5), uint32(10), int64(-1), uint32(432000), uint32(20), false)
	f.Add(int64(1), uint32(10), int64(7), uint32(3), uint32(11), false)
	f.Fuzz(func(t *testing.T, current int64, last uint32, maxMana int64, regen uint32, now uint32, skipCap bool) {
		if regen == 0 {
			return // manabar_params::validate rejects regen_time 0
		}
		want, wantUpdate, ok := hivedRegenerateMana(current, last, maxMana, regen, now, skipCap)
		if !ok {
			// hived throws; gopherd keeps the old bar (now < last) or
			// saturates (a regeneration above INT64_MAX, reachable only by
			// an RC bar of ~1e17 left untouched for over 2.5 years). hived
			// cannot put such a transaction in a block, so applying blocks
			// never meets it; a gopherd witness admitting one would.
			return
		}
		got, gotUpdate := RegenerateMana(current, last, ManabarParams{MaxMana: maxMana, RegenTime: regen}, now, skipCap)
		if got != want || gotUpdate != wantUpdate {
			t.Fatalf("RegenerateMana(cur=%d last=%d max=%d regen=%d now=%d skip=%v) = (%d, %d), hived (%d, %d)",
				current, last, maxMana, regen, now, skipCap, got, gotUpdate, want, wantUpdate)
		}
	})
}
