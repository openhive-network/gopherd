package main

import (
	"testing"

	"gopherd/internal/state/objects"
)

// TestRCCacheDrift pins the invariant validate-state reports, which is hived's
// HF29 RC_IS_CONSENSUS assertion expressed as a state check: an account's
// cached last_max_rc must equal the maximum its VESTS and RC delegations
// imply. A snapshot with drift is a snapshot on which that assertion would
// wedge the node the moment the fork activates.
func TestRCCacheDrift(t *testing.T) {
	t.Run("untouched accounts are not checked", func(t *testing.T) {
		// last_max_rc == 0 means the RC subsystem never touched this account.
		// Counting it as drift would drown the real signal: the chain is full
		// of pre-HF20 and never-funded accounts.
		a := &objects.Account{Name: "alice", VestingShares: 1_000_000, LastMaxRC: 0}
		if _, checked := rcCacheDrift(a); checked {
			t.Error("an account with last_max_rc == 0 must be excluded")
		}
	})

	t.Run("a consistent account has no drift", func(t *testing.T) {
		a := &objects.Account{Name: "alice", VestingShares: 1_000_000}
		a.LastMaxRC = a.MaximumRC()
		d, checked := rcCacheDrift(a)
		if !checked {
			t.Fatal("an RC-touched account must be checked")
		}
		if d != 0 {
			t.Errorf("drift: got %d, want 0", d)
		}
	})

	t.Run("vesting gained without restamping shows positive drift", func(t *testing.T) {
		a := &objects.Account{Name: "alice", VestingShares: 1_000_000}
		a.LastMaxRC = a.MaximumRC()
		a.VestingShares += 500 // an operation moved VESTS and forgot the cache
		d, checked := rcCacheDrift(a)
		if !checked {
			t.Fatal("must be checked")
		}
		if d != 500 {
			t.Errorf("drift: got %d, want +500", d)
		}
	})

	t.Run("an RC delegation out shows negative drift", func(t *testing.T) {
		a := &objects.Account{Name: "alice", VestingShares: 1_000_000}
		a.LastMaxRC = a.MaximumRC()
		a.DelegatedRCOut = 400
		d, checked := rcCacheDrift(a)
		if !checked {
			t.Fatal("must be checked")
		}
		if d != -400 {
			t.Errorf("drift: got %d, want -400", d)
		}
	})
}

func TestAbs64(t *testing.T) {
	for _, tc := range []struct{ in, want int64 }{
		{0, 0}, {5, 5}, {-5, 5},
	} {
		if got := abs64(tc.in); got != tc.want {
			t.Errorf("abs64(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
