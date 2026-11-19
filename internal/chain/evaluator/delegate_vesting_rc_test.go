package evaluator

import (
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state/objects"
)

// TestDelegateVestingRegeneratesRCFirst pins hive_evaluator_transfer.cpp:738-739:
// delegate_vesting_shares regenerates both sides' RC at the maximum each held
// before the delegation, then update_account_after_vest_change hands the
// delegatee its new headroom and clamps the delegator. Without the
// regeneration both kept their old last_update and the gap was later credited
// at the new maxima.
func TestDelegateVestingRegeneratesRCFirst(t *testing.T) {
	const day = uint32(24 * 3600)
	s := drcSession(t)
	before := map[string]objects.Account{}
	for _, name := range []string{"alice", "bob"} {
		a, err := objects.GetAccount(s, auth.AccountName(name))
		if err != nil {
			t.Fatal(err)
		}
		a.RCManaLastUpdate = drcNow - day
		a.RCManaCurrent = a.LastMaxRC / 4
		objects.PutAccount(s, &a)
		objects.RegenerateRCMana(&a, drcNow) // what hived does first
		before[name] = a
	}

	o := op.DelegateVestingShares{
		Delegator:     "alice",
		Delegatee:     "bob",
		VestingShares: asset.Asset{Amount: drcAliceVests / 2, Symbol: asset.Symbol{Num: asset.VestsSymbol}},
	}
	if err := ApplyOp(drcCtx(s, false, false), op.FromOperation(&o)); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"alice", "bob"} {
		got, err := objects.GetAccount(s, auth.AccountName(name))
		if err != nil {
			t.Fatal(err)
		}
		regen := before[name]
		newMax := got.MaximumRC()
		want := regen.RCManaCurrent + max(newMax-regen.LastMaxRC, 0)
		want = min(want, newMax)
		if got.RCManaLastUpdate != drcNow {
			t.Errorf("%s: rc last update %d, want %d", name, got.RCManaLastUpdate, drcNow)
		}
		if got.RCManaCurrent != want {
			t.Errorf("%s: rc mana %d, want %d", name, got.RCManaCurrent, want)
		}
		if got.LastMaxRC != newMax {
			t.Errorf("%s: last_max_rc %d, want %d", name, got.LastMaxRC, newMax)
		}
	}
}
