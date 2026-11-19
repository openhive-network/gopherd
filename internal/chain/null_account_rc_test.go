package chain

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

// TestClearNullAccountRefreshesRC pins database.cpp:1245-1254: burning null's
// VESTS regenerates its RC before and refreshes last_max_rc after. Without the
// refresh null kept the maximum of VESTS it no longer held, so the next
// transfer_to_vesting to null regenerated against a stale last_max_rc, the
// condition HF29's RC_IS_CONSENSUS refuses.
func TestClearNullAccountRefreshesRC(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}

	const now = uint32(1_600_000_000)
	blockNum := hardfork.MainnetActivationBlock[hardfork.HF20] + 1000

	s := state.NewSession(db)
	null, err := objects.GetAccount(s, nullAccountName)
	if err != nil {
		t.Fatal(err)
	}
	null.VestingShares = 5_000_000
	null.LastMaxRC = null.MaximumRC()
	null.RCManaCurrent = null.LastMaxRC
	null.RCManaLastUpdate = now - 100
	objects.PutAccount(s, &null)
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		t.Fatal(err)
	}
	dgpo.Time = now
	dgpo.TotalVestingShares += null.VestingShares
	dgpo.TotalVestingFundHive += 5_000
	dgpo.CurrentSupply += 5_000
	dgpo.VirtualSupply += 5_000
	objects.PutDGPO(s, &dgpo)

	clearNullAccountBalance(s, blockNum, nil)

	got, err := objects.GetAccount(s, nullAccountName)
	if err != nil {
		t.Fatal(err)
	}
	if got.VestingShares != 0 {
		t.Fatalf("null still holds %d VESTS", got.VestingShares)
	}
	if want := got.MaximumRC(); got.LastMaxRC != want {
		t.Errorf("last_max_rc %d after the burn, want the new maximum %d", got.LastMaxRC, want)
	}
	if got.RCManaLastUpdate != now {
		t.Errorf("rc last update %d, want %d (regenerated before the burn)", got.RCManaLastUpdate, now)
	}
	if got.RCManaCurrent > got.LastMaxRC {
		t.Errorf("rc mana %d above max %d", got.RCManaCurrent, got.LastMaxRC)
	}

	before := objects.RCStaleMaxCount()
	objects.RegenerateRCMana(&got, now+3)
	if n := objects.RCStaleMaxCount() - before; n != 0 {
		t.Errorf("regenerating null after the burn counted %d stale maxima, want 0", n)
	}
}
