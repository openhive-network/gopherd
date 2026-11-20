package objects

import (
	"errors"
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

// FuzzRegenerateManaMatchesHived compares RegenerateManaChecked with hived's
// regenerate_mana on every input. Where hived returns, the two must agree
// exactly: voting and RC mana feed vote weights and the RC stamps, so a
// one-unit difference is a fork. Where hived throws on an overflowing
// regeneration, gopherd must return ErrManaRegenOverflow and leave the bar
// alone, so the transaction (or block) fails on both.
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
		got, gotUpdate, err := RegenerateManaChecked(current, last, ManabarParams{MaxMana: maxMana, RegenTime: regen}, now, skipCap)
		if !ok {
			if now < last {
				// hived asserts now >= last_update_time; gopherd keeps the
				// bar. No caller passes a time before the bar's own stamp.
				return
			}
			if !errors.Is(err, ErrManaRegenOverflow) || got != current || gotUpdate != last {
				t.Fatalf("RegenerateManaChecked(cur=%d last=%d max=%d regen=%d now=%d skip=%v) = (%d, %d, %v), hived throws",
					current, last, maxMana, regen, now, skipCap, got, gotUpdate, err)
			}
			return
		}
		if err != nil || got != want || gotUpdate != wantUpdate {
			t.Fatalf("RegenerateManaChecked(cur=%d last=%d max=%d regen=%d now=%d skip=%v) = (%d, %d, %v), hived (%d, %d)",
				current, last, maxMana, regen, now, skipCap, got, gotUpdate, err, want, wantUpdate)
		}
		if g, u := RegenerateMana(current, last, ManabarParams{MaxMana: maxMana, RegenTime: regen}, now, skipCap); g != got || u != gotUpdate {
			t.Fatalf("RegenerateMana = (%d, %d), RegenerateManaChecked (%d, %d)", g, u, got, gotUpdate)
		}
	})
}

// dormantRCOverflowAge returns the shortest idle time, in seconds, after which
// regenerating an RC bar with maximum maxRC overflows in hived: the smallest
// dt with maxRC * dt / 432000 > INT64_MAX.
func dormantRCOverflowAge(maxRC int64) uint32 {
	n := new(big.Int).Mul(new(big.Int).Add(big.NewInt(math.MaxInt64), big.NewInt(1)), big.NewInt(int64(RCRegenTimeSec)))
	q, r := new(big.Int).QuoRem(n, big.NewInt(maxRC), new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return uint32(q.Int64())
}

// TestRegenerateRCManaOverflowFailsAsHived pins hived's u_regen <= INT64_MAX
// assertion for an account-sized case: 100 billion VESTS (1e17 raw) left
// untouched for about 461 days. One second earlier the bar regenerates to its
// maximum; from then on hived throws, so gopherd must refuse rather than
// saturate (which accepted a transaction every hived node rejects).
func TestRegenerateRCManaOverflowFailsAsHived(t *testing.T) {
	const now = uint32(1_700_000_000)
	a := Account{Name: "whale", VestingShares: 100_000_000_000_000_000}
	maxRC := a.MaximumRC()
	age := dormantRCOverflowAge(maxRC)
	if age < 365*86400 || age > 2*365*86400 {
		t.Fatalf("overflow age %d s for max %d: the fixture is not the case described", age, maxRC)
	}
	a.LastMaxRC = maxRC

	a.RCManaCurrent, a.RCManaLastUpdate = 0, now-(age-1)
	if err := RegenerateRCMana(&a, now); err != nil {
		t.Fatalf("idle %d s: %v, want a full bar", age-1, err)
	}
	if a.RCManaCurrent != maxRC || a.RCManaLastUpdate != now {
		t.Fatalf("idle %d s: mana %d @%d, want %d @%d", age-1, a.RCManaCurrent, a.RCManaLastUpdate, maxRC, now)
	}

	a.RCManaCurrent, a.RCManaLastUpdate = 0, now-age
	err := RegenerateRCMana(&a, now)
	if !errors.Is(err, ErrManaRegenOverflow) {
		t.Fatalf("idle %d s: err %v, want ErrManaRegenOverflow", age, err)
	}
	if a.RCManaCurrent != 0 || a.RCManaLastUpdate != now-age {
		t.Errorf("failed regeneration changed the bar to %d @%d", a.RCManaCurrent, a.RCManaLastUpdate)
	}
	if err := RegenerateRCManabar(&a, now); !errors.Is(err, ErrManaRegenOverflow) {
		t.Errorf("RegenerateRCManabar: err %v, want ErrManaRegenOverflow", err)
	}
	if err := UpdateAccountAfterRCDelegation(&a, now, 1, true); !errors.Is(err, ErrManaRegenOverflow) {
		t.Errorf("UpdateAccountAfterRCDelegation: err %v, want ErrManaRegenOverflow", err)
	}

	// A full bar returns before the multiplication, as in hived.
	a.RCManaCurrent, a.RCManaLastUpdate = maxRC, now-age
	if err := RegenerateRCMana(&a, now); err != nil {
		t.Errorf("full bar idle %d s: %v", age, err)
	}
}
