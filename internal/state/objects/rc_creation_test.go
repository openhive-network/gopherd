package objects

import "testing"

// TestNewAccountRCSeededFromFee pins the rule that made us refuse ~1
// transaction per block that the network mined.
//
// hived seeds a new account's rc_adjustment from the creation FEE converted to
// VESTS, and when that adjustment is non-zero the RC manabar starts FULL
// (account_object.hpp:50-59, driven by hive_evaluator_account.cpp:159-168).
//
// We did neither: rc_adjustment was only ever set by the HF20 loop, which
// touches accounts existing AT HF20. Every account created afterwards
// therefore had rc_adjustment=0, and with little or no VESTS that means
// max_rc=0 — it could not transact at all in our view while transacting
// happily on mainnet. Observed live for "mcbot" (zero VESTS):
// `WE-HAD-IT-AND-DROPPED=1 [51f4bcf050a2…=insufficient-rc]`, in a block the
// real witness produced WITH that transaction in it.
func TestNewAccountRCSeededFromFee(t *testing.T) {
	const now uint32 = 1_600_000_000

	// A brand-new account with NO vesting shares — the exact shape that broke.
	var a Account
	const adjustment int64 = 2_020_748_973

	InitRCManabarWithAdjustment(&a, adjustment, now)

	if a.RCAdjustment != adjustment {
		t.Fatalf("rc_adjustment: got %d, want %d — without it a VEST-less account has no RC at all",
			a.RCAdjustment, adjustment)
	}
	maxRC := a.MaximumRC()
	if maxRC <= 0 {
		t.Fatalf("max_rc must be positive once the fee-derived adjustment is applied, got %d", maxRC)
	}
	if a.LastMaxRC != maxRC {
		t.Fatalf("last_max_rc: got %d, want %d", a.LastMaxRC, maxRC)
	}
	// The half that matters most: C++ starts the bar FULL, not empty. Starting
	// at 0 would still refuse the account's first transactions, since RC
	// regenerates linearly over five days.
	if a.RCManaCurrent != maxRC {
		t.Fatalf("current_mana must start FULL (%d), got %d — a 0 start refuses the account for days",
			maxRC, a.RCManaCurrent)
	}
	if a.RCManaLastUpdate != now {
		t.Fatalf("last_update_time: got %d, want %d", a.RCManaLastUpdate, now)
	}
}

// TestNewAccountZeroAdjustmentStaysEmpty pins the other branch: C++ only fills
// the manabar `if( rc_adjustment.value )`. A zero adjustment must NOT silently
// grant mana, or we would over-credit accounts hived leaves empty and start
// including transactions the network refuses — the opposite failure.
func TestNewAccountZeroAdjustmentStaysEmpty(t *testing.T) {
	const now uint32 = 1_600_000_000
	var a Account

	InitRCManabarWithAdjustment(&a, 0, now)

	if a.RCAdjustment != 0 {
		t.Fatalf("rc_adjustment must stay 0, got %d", a.RCAdjustment)
	}
	if a.RCManaCurrent != 0 {
		t.Fatalf("current_mana must stay 0 when there is no adjustment, got %d", a.RCManaCurrent)
	}
	if a.RCManaLastUpdate != now {
		t.Fatalf("last_update_time must still be stamped so regen has an origin, got %d", a.RCManaLastUpdate)
	}
}
