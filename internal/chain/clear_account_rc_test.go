package chain

import (
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

// TestClearAccountRefreshesRC pins database.cpp:1517-1586: clear_account
// regenerates the cleared account's RC and each delegatee's before their
// VESTS change and refreshes last_max_rc after. The delegatees of the HF23
// accounts kept transacting, and their mana is hashed into the RC stamp
// whenever they pay for a transaction.
func TestClearAccountRefreshesRC(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}

	const now = uint32(1_584_000_000)
	const day = uint32(24 * 3600)
	const delegated = int64(400_000_000_000)

	s := state.NewSession(db)
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		t.Fatal(err)
	}
	dgpo.Time = now
	dgpo.TotalVestingShares += 1_100_000_000_000
	dgpo.TotalVestingFundHive += 1_100_000_000
	objects.PutDGPO(s, &dgpo)

	mk := func(name string, vests, delegOut, recv int64) objects.Account {
		a := objects.Account{
			Name:                   auth.AccountName(name),
			VestingShares:          vests,
			DelegatedVestingShares: delegOut,
			ReceivedVestingShares:  recv,
			RCManaLastUpdate:       now - day,
		}
		a.LastMaxRC = a.MaximumRC()
		a.RCManaCurrent = a.LastMaxRC / 4
		objects.PutAccount(s, &a)
		return a
	}
	eve := mk("eve", 1_000_000_000_000, delegated, 0)
	frank := mk("frank", 100_000_000_000, 0, delegated)
	objects.PutVestingDelegation(s, &objects.VestingDelegation{Delegator: "eve", Delegatee: "frank", VestingShares: delegated})

	// hived regenerates both at their pre-clear maxima first.
	objects.RegenerateRCMana(&eve, now)
	objects.RegenerateRCMana(&frank, now)

	if err := ClearAccount(s, "eve"); err != nil {
		t.Fatal(err)
	}

	for _, want := range []objects.Account{eve, frank} {
		got, err := objects.GetAccount(s, string(want.Name))
		if err != nil {
			t.Fatal(err)
		}
		newMax := got.MaximumRC()
		wantMana := min(want.RCManaCurrent+max(newMax-want.LastMaxRC, 0), newMax)
		if got.RCManaLastUpdate != now {
			t.Errorf("%s: rc last update %d, want %d", want.Name, got.RCManaLastUpdate, now)
		}
		if got.RCManaCurrent != wantMana {
			t.Errorf("%s: rc mana %d, want %d", want.Name, got.RCManaCurrent, wantMana)
		}
		if got.LastMaxRC != newMax {
			t.Errorf("%s: last_max_rc %d, want %d", want.Name, got.LastMaxRC, newMax)
		}
	}
}
