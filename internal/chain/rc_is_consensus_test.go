package chain

import (
	"errors"
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

// TestRCIsConsensusGate covers HF29 RC_IS_CONSENSUS (hived cbd08b3ce,
// rc_utility.cpp:567), which promotes "payer ran out of RC" from an
// admission-time refusal to a consensus rule:
//
//	if( db.is_in_control() || db.is_reapplying_one_tx()
//	    || db.has_hardfork( HIVE_HARDFORK_1_29_RC_IS_CONSENSUS ) )
//
// Before HF29, a block containing an underfunded transaction still applies on
// every node that is merely PROCESSING it — the shortfall is logged, not
// enforced. After HF29 every node must reject it. gopherd's `enforce` flag is
// the equivalent of hived's first two conditions (block assembly passes true;
// replay passes false), so the hardfork term is what this test pins.
//
// The hardfork Set is injected rather than derived from a block number on
// purpose: HF29 has no activation block, so SetForBlock can never produce a
// Set containing it and the post-fork branch would otherwise be unreachable.
func TestRCIsConsensusGate(t *testing.T) {
	// HF20 must be active for RC to apply at all.
	blockNum := hardfork.MainnetActivationBlock[hardfork.HF20] + 1000

	// An account with vesting but a drained manabar: any RC cost exceeds it.
	setup := func(t *testing.T) *state.Session {
		t.Helper()
		db, err := state.Open(t.TempDir(), 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := genesis.Apply(db); err != nil {
			t.Fatal(err)
		}
		s := state.NewSession(db)
		// RC pricing needs three things or it charges ZERO — "when rc_regen is
		// 0, everything is free" — and applyRCForTx returns at its `cost <= 0`
		// guard long before the manabar check this test is about:
		//   1. an RC pool + resource params (initRCBootstrapHF20 — rcSetPoolParams
		//      alone leaves resource params absent and all pool weights zero), and
		//   2. non-zero global vesting, since
		//      rcRegen = dgpo.TotalVestingShares / rcRegenBlocks.
		initRCBootstrapHF20(s, 1_600_000_000)
		dgpo, err := objects.GetDGPO(s)
		if err != nil {
			t.Fatal(err)
		}
		dgpo.TotalVestingShares = 500_000_000_000_000
		objects.PutDGPO(s, &dgpo)
		objects.PutAccount(s, &objects.Account{
			Name:          "alice",
			VestingShares: 1_000_000,
			Balance:       1_000_000,
			// Drained manabar, last updated AT the block time so
			// RegenerateRCMana has zero elapsed time to refill from —
			// otherwise the bar regenerates to full and no cost can exceed it.
			RCManaCurrent:    0,
			RCManaLastUpdate: 1_600_000_000,
			LastMaxRC:        1_000_000,
		})
		objects.PutAccount(s, &objects.Account{Name: "bob"})
		if err := s.Commit(false); err != nil {
			t.Fatal(err)
		}
		return state.NewSession(db)
	}

	// A transaction that costs RC.
	signed := &tx.SignedTransaction{
		Transaction: tx.Transaction{
			Operations: []op.Op{op.FromOperation(&op.Transfer{
				From: "alice", To: "bob",
				Amount: asset.Asset{Amount: 1, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
				Memo:   "rc",
			})},
		},
	}

	preHF29 := hardfork.SetForBlock(blockNum)
	postHF29 := preHF29 | hardfork.Set(1<<hardfork.HF29)
	if preHF29.Has(hardfork.HF29) {
		t.Fatal("setup: HF29 must not be reachable through SetForBlock")
	}
	if !preHF29.Has(hardfork.HF20) {
		t.Fatal("setup: HF20 must be active or RC never runs")
	}

	cases := []struct {
		name       string
		enforce    bool
		hf         hardfork.Set
		wantReject bool
	}{
		{"replaying history before the fork", false, preHF29, false},
		{"block assembly before the fork (enforce)", true, preHF29, true},
		{"replaying a block after the fork", false, postHF29, true},
		{"block assembly after the fork", true, postHF29, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ApplyRCForTxForTest(setup(t), signed, blockNum, 1_600_000_000, 200, tc.enforce, tc.hf)
			if tc.wantReject {
				if !errors.Is(err, ErrInsufficientRC) {
					t.Fatalf("got %v, want ErrInsufficientRC", err)
				}
				return
			}
			if errors.Is(err, ErrInsufficientRC) {
				t.Fatal("insufficient RC rejected while replaying pre-HF29 history; such blocks exist and must still apply")
			}
		})
	}
}
