package objects

import "testing"

// TestVestGainCreditsRCMana pins hived's update_account_after_vest_change
// behaviour: a POSITIVE change in maximum RC is credited straight to current
// mana so the account can use it immediately.
//
// Regression guard for two distinct bugs:
//   - the credit was missing entirely, so an account funded after creation had
//     a billion-RC ceiling and zero usable mana for five days;
//   - the first fix captured the old ceiling AFTER RegenerateRCMana, which
//     assigns LastMaxRC itself, making the delta always zero.
func TestVestGainCreditsRCMana(t *testing.T) {
	a := &Account{Name: "alice"}
	a.RCManaCurrent = 0
	a.LastMaxRC = 0
	a.RCManaLastUpdate = 1000

	// Account gains vesting.
	a.VestingShares = 5_000_000_000
	HandleVestingWithdrawalRCImpact(nil, a, 1000)

	if a.RCManaCurrent < 4_900_000_000 {
		t.Fatalf("mana = %d after gaining 5e9 VESTS, want it credited to ~max (%d)",
			a.RCManaCurrent, a.LastMaxRC)
	}
	if a.RCManaCurrent > a.LastMaxRC {
		t.Fatalf("mana %d exceeds max %d", a.RCManaCurrent, a.LastMaxRC)
	}

	// A DECREASE must NOT reduce mana: power-down and reduced delegation both
	// carry a delay longer than full regeneration, so hived skips the negative
	// delta rather than risk an RC pump.
	before := a.RCManaCurrent
	a.VestingShares = 1_000_000_000
	HandleVestingWithdrawalRCImpact(nil, a, 1000)
	if a.RCManaCurrent > before {
		t.Fatalf("mana rose on a vesting DECREASE: %d -> %d", before, a.RCManaCurrent)
	}
}
