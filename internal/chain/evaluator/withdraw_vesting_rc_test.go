package evaluator

import (
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state/objects"
)

// TestWithdrawVestingRegeneratesRCFirst pins hive_evaluator_transfer.cpp:357:
// withdraw_vesting regenerates RC mana against the maximum that held before
// the power down changes it, on both the start and the cancel path. Without
// it the account kept its old last_update, and the next regeneration credited
// the whole gap at the new maximum: different mana from hived's for an
// account whose mana feeds the RC stamp as soon as it pays for a transaction.
func TestWithdrawVestingRegeneratesRCFirst(t *testing.T) {
	const vests = int64(1_300_000_000_000)
	const day = uint32(24 * 3600)
	for _, tc := range []struct {
		name   string
		amount int64
		rate   int64 // existing power down, 0 for none
	}{
		{"start", vests, 0},
		{"cancel", 0, vests / 13},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := drcSession(t)
			a := objects.Account{
				Name:             "dave",
				VestingShares:    vests,
				RCManaLastUpdate: drcNow - day,
			}
			if tc.rate != 0 {
				a.VestingWithdrawRate = tc.rate
				a.NextVestingWithdrawal = drcNow + 3*day
				a.ToWithdraw = vests
			}
			a.LastMaxRC = a.MaximumRC()
			a.RCManaCurrent = a.LastMaxRC / 4
			objects.PutAccount(s, &a)

			// What hived computes: regenerate at the old maximum, then
			// update_account_after_vest_change(fill=false) clamps to the new.
			want := a
			objects.RegenerateRCMana(&want, drcNow)

			o := op.WithdrawVesting{
				Account:       auth.AccountName("dave"),
				VestingShares: asset.Asset{Amount: tc.amount, Symbol: asset.Symbol{Num: asset.VestsSymbol}},
			}
			if err := ApplyOp(drcCtx(s, false, false), op.FromOperation(&o)); err != nil {
				t.Fatal(err)
			}
			got, err := objects.GetAccount(s, "dave")
			if err != nil {
				t.Fatal(err)
			}
			if got.RCManaLastUpdate != drcNow {
				t.Errorf("rc last update %d, want %d (regenerated before the change)", got.RCManaLastUpdate, drcNow)
			}
			wantMana := min(want.RCManaCurrent, got.MaximumRC())
			if got.RCManaCurrent != wantMana {
				t.Errorf("rc mana %d, want %d", got.RCManaCurrent, wantMana)
			}
			if got.LastMaxRC != got.MaximumRC() {
				t.Errorf("last_max_rc %d, want %d", got.LastMaxRC, got.MaximumRC())
			}
		})
	}
}
