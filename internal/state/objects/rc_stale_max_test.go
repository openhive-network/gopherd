package objects

import "testing"

// TestRegenerateRCManaCountsStaleMax covers the condition behind hived's
// "max RC changed without triggering an op" NOTIFYALERT (rc_utility.cpp:353),
// which HF29's RC_IS_CONSENSUS promotes to a throw.
//
// gopherd cannot port that assertion honestly yet: RegenerateRCMana REPAIRS
// last_max_rc rather than complaining, so the condition has never been
// observed here, and hived's own FC_TODO says it "needs to be checked with
// replay" before it goes unconditional. Counting is what makes that check
// possible — a clean replay reporting 0 is the evidence required.
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
	// The repair still happens — behaviour is unchanged, only visibility.
	if a.LastMaxRC != a.MaximumRC() {
		t.Errorf("LastMaxRC not refreshed: got %d, want %d", a.LastMaxRC, a.MaximumRC())
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
