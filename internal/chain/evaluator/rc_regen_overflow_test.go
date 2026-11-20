package evaluator

import (
	"errors"
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state/objects"
)

// rcOverflowIdle is how long a 1e17-VESTS bar must sit untouched before
// hived's RC regeneration overflows (max * dt / 432000 > INT64_MAX): 461 days.
const rcOverflowIdle = 39_844_968

// TestTransferToVestingIntoDormantWhaleFails pins hived's u_regen <= INT64_MAX
// assertion on an operation path. transfer_to_vesting regenerates the
// receiver's RC (database.cpp:1023), and for an account with 100 billion
// VESTS idle for 461 days that regeneration throws in hived, so the
// transaction is invalid. gopherd saturated instead and accepted it: a
// gopherd witness would have produced a block every hived node rejects.
// The regeneration runs inside a mutate closure, so this also covers the
// session fault that ApplyOp turns into the operation's error.
func TestTransferToVestingIntoDormantWhaleFails(t *testing.T) {
	for _, tc := range []struct {
		idle   uint32
		wantOK bool
	}{
		{rcOverflowIdle - 1, true},
		{rcOverflowIdle, false},
	} {
		s := drcSession(t)
		bob, err := objects.GetAccount(s, "bob")
		if err != nil {
			t.Fatal(err)
		}
		bob.VestingShares = 100_000_000_000_000_000
		bob.LastMaxRC = bob.MaximumRC()
		bob.RCManaCurrent = 0
		bob.RCManaLastUpdate = drcNow - tc.idle
		objects.PutAccount(s, &bob)
		alice, err := objects.GetAccount(s, "alice")
		if err != nil {
			t.Fatal(err)
		}
		alice.Balance = 10_000
		objects.PutAccount(s, &alice)
		// create_vesting takes its hardfork from the DGPO's head block.
		dgpo, err := objects.GetDGPO(s)
		if err != nil {
			t.Fatal(err)
		}
		dgpo.HeadBlockNumber = hardfork.MainnetActivationBlock[hardfork.HF28] + 1
		objects.PutDGPO(s, &dgpo)

		o := op.TransferToVesting{
			From:   "alice",
			To:     "bob",
			Amount: asset.Asset{Amount: 1_000, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
		}
		err = ApplyOp(drcCtx(s, false, false), op.FromOperation(&o))
		switch {
		case tc.wantOK && err != nil:
			t.Errorf("idle %d s: %v, want success", tc.idle, err)
		case !tc.wantOK && !errors.Is(err, objects.ErrManaRegenOverflow):
			t.Errorf("idle %d s: err %v, want ErrManaRegenOverflow", tc.idle, err)
		}
		s.Close()
	}
}

// TestDelegateVestingSharesToDormantWhaleFails covers the plain error path:
// delegate_vesting_shares regenerates the delegatee (hive_evaluator.cpp,
// regenerate_rc_mana on both sides) and must fail where hived throws.
func TestDelegateVestingSharesToDormantWhaleFails(t *testing.T) {
	s := drcSession(t)
	defer s.Close()
	bob, err := objects.GetAccount(s, "bob")
	if err != nil {
		t.Fatal(err)
	}
	bob.VestingShares = 100_000_000_000_000_000
	bob.LastMaxRC = bob.MaximumRC()
	bob.RCManaCurrent = 0
	bob.RCManaLastUpdate = drcNow - rcOverflowIdle
	objects.PutAccount(s, &bob)

	o := op.DelegateVestingShares{
		Delegator:     "alice",
		Delegatee:     "bob",
		VestingShares: asset.Asset{Amount: 10_000_000, Symbol: asset.Symbol{Num: asset.VestsSymbol}},
	}
	err = ApplyOp(drcCtx(s, false, false), op.FromOperation(&o))
	if !errors.Is(err, objects.ErrManaRegenOverflow) {
		t.Errorf("err %v, want ErrManaRegenOverflow", err)
	}
}
