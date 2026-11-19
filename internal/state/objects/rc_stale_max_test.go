package objects

import "testing"

// TestRegenerateRCManaCountsStaleMax covers the condition behind hived's
// "max RC changed without triggering an op" NOTIFYALERT (rc_utility.cpp:353),
// which HF29's RC_IS_CONSENSUS promotes to a throw.
//
// hived only warns and leaves last_max_rc stale until the next
// update_account_after_vest_change, which credits new_max - last_max_rc;
// gopherd counts the condition and, like hived, does not repair it. hived's
// own FC_TODO says the assertion "needs to be checked with replay" before it
// goes unconditional; the count is that check.
func TestRegenerateRCManaCountsStaleMax(t *testing.T) {
	before := RCStaleMaxCount()

	// An account whose stored LastMaxRC disagrees with its computed maximum:
	// vesting says one thing, the cached cap another.
	a := &Account{
		Name:          "alice",
		VestingShares: 1_000_000,
		LastMaxRC:     12_345, // stale
		RCManaCurrent: 1,
		// Non-zero so RegenerateMana has elapsed time to work with.
		RCManaLastUpdate: 1_600_000_000,
	}
	if a.MaximumRC() == a.LastMaxRC {
		t.Fatal("setup: the two must disagree for this test to mean anything")
	}
	RegenerateRCMana(a, 1_600_000_100)

	if got := RCStaleMaxCount() - before; got != 1 {
		t.Errorf("stale-max count: got +%d, want +1", got)
	}
	// Left stale, as hived leaves it: the next vest-change refresh credits
	// the difference from the stale value.
	if a.LastMaxRC != 12_345 {
		t.Errorf("LastMaxRC rewritten to %d, want it left at 12345", a.LastMaxRC)
	}
	mana := a.RCManaCurrent
	UpdateAccountAfterVestChange(a, 1_600_000_100, true)
	if want := min(mana+a.MaximumRC()-12_345, a.MaximumRC()); a.RCManaCurrent != want {
		t.Errorf("mana after the refresh %d, want %d", a.RCManaCurrent, want)
	}

	// The payer's regeneration neither counts nor touches last_max_rc.
	before = RCStaleMaxCount()
	payer := &Account{Name: "dave", VestingShares: 1_000_000, LastMaxRC: 12_345, RCManaLastUpdate: 1_600_000_000}
	RegenerateRCManabar(payer, 1_600_000_100)
	if got := RCStaleMaxCount() - before; got != 0 || payer.LastMaxRC != 12_345 {
		t.Errorf("RegenerateRCManabar counted +%d and left last_max_rc %d, want +0 and 12345", got, payer.LastMaxRC)
	}

	// An untouched account (LastMaxRC == 0) is not stale, it is uninitialised:
	// pre-HF20 history and freshly created accounts are full of these and must
	// not be counted.
	before = RCStaleMaxCount()
	fresh := &Account{Name: "bob", VestingShares: 5_000, RCManaLastUpdate: 1_600_000_000}
	RegenerateRCMana(fresh, 1_600_000_100)
	if got := RCStaleMaxCount() - before; got != 0 {
		t.Errorf("uninitialised account counted as stale: got +%d, want 0", got)
	}

	// An account already in agreement is not counted either.
	before = RCStaleMaxCount()
	agreed := &Account{Name: "carol", VestingShares: 7_000, RCManaLastUpdate: 1_600_000_000}
	agreed.LastMaxRC = agreed.MaximumRC()
	RegenerateRCMana(agreed, 1_600_000_100)
	if got := RCStaleMaxCount() - before; got != 0 {
		t.Errorf("consistent account counted as stale: got +%d, want 0", got)
	}
}
