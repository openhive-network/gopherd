package evaluator

import (
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state/objects"
)

// TestTransferToVestingLeavesSenderRC pins that transfer_to_vesting does not
// regenerate the sender's RC: hived regenerates only the receiver, inside
// adjust_account_vesting_balance. An extra regeneration of a sender that is
// not the transaction's payer splits its regeneration interval, and the
// rounding can leave its mana a unit below hived's.
func TestTransferToVestingLeavesSenderRC(t *testing.T) {
	const day = uint32(24 * 3600)
	s := drcSession(t)
	alice, err := objects.GetAccount(s, "alice")
	if err != nil {
		t.Fatal(err)
	}
	alice.Balance = 10_000
	alice.RCManaLastUpdate = drcNow - day
	alice.RCManaCurrent = alice.LastMaxRC / 3
	objects.PutAccount(s, &alice)

	o := op.TransferToVesting{
		From:   "alice",
		To:     "bob",
		Amount: asset.Asset{Amount: 1_000, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
	}
	if err := ApplyOp(drcCtx(s, false, false), op.FromOperation(&o)); err != nil {
		t.Fatal(err)
	}
	got, err := objects.GetAccount(s, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.RCManaLastUpdate != alice.RCManaLastUpdate || got.RCManaCurrent != alice.RCManaCurrent {
		t.Errorf("sender RC changed to %d @%d, want untouched %d @%d",
			got.RCManaCurrent, got.RCManaLastUpdate, alice.RCManaCurrent, alice.RCManaLastUpdate)
	}
	bob, err := objects.GetAccount(s, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if bob.RCManaLastUpdate != drcNow {
		t.Errorf("receiver rc last update %d, want %d", bob.RCManaLastUpdate, drcNow)
	}
}
