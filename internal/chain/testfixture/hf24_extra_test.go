package testfixture

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// TestHF24ConsolidateBalance ports hf24_tests.cpp::consolidate_balance.
// At HF24, balances on the OLD treasury account ("steem.dao") are
// transferred to the NEW one ("hive.fund"). After HF24 activation:
//   - steem.dao should have zero across all balance fields
//   - hive.fund should have the consolidated balances
//
// We pre-seed steem.dao with HIVE/HBD balances, then run HF24, then
// verify the consolidation.
func TestHF24ConsolidateBalance(t *testing.T) {
	f := New(t)
	// Get to HF21 first so steem.dao is created.
	f.HardforksUpTo(hardfork.HF21)

	// Verify steem.dao exists.
	if has, _ := objects.HasAccount(state.NewSession(f.State), "steem.dao"); !has {
		t.Fatal("expected steem.dao to exist post-HF21")
	}

	// Seed steem.dao with HIVE + HBD.
	{
		s := state.NewSession(f.State)
		td, _ := objects.GetAccount(s, "steem.dao")
		td.Balance = 5000
		td.HBDBalance = 5000
		objects.PutAccount(s, &td)
		if err := s.Commit(false); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Activate HF24 — should move balances to hive.fund.
	f.HardforksUpTo(hardfork.HF24)

	// Verify steem.dao zeroed.
	old, err := objects.GetAccount(state.NewSession(f.State), "steem.dao")
	if err == nil {
		if old.Balance != 0 {
			t.Errorf("steem.dao.Balance: got %d, want 0 (consolidated)", old.Balance)
		}
		if old.HBDBalance != 0 {
			t.Errorf("steem.dao.HBDBalance: got %d, want 0 (consolidated)", old.HBDBalance)
		}
	}

	// Verify hive.fund got the funds.
	newT, err := objects.GetAccount(state.NewSession(f.State), "hive.fund")
	if err != nil {
		t.Fatalf("hive.fund: %v", err)
	}
	if newT.Balance != 5000 {
		t.Errorf("hive.fund.Balance: got %d, want 5000", newT.Balance)
	}
	if newT.HBDBalance != 5000 {
		t.Errorf("hive.fund.HBDBalance: got %d, want 5000", newT.HBDBalance)
	}
}

// TestHF26HBDPercentageJump ports the HBD-percentage portion of
// hf26_tests.cpp. HF26 raises both HBD start/stop percent to 20%.
func TestHF26HBDPercentageJump(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF25)

	pre := f.DGPO()
	if pre.HBDStartPercent == 2000 || pre.HBDStopPercent == 2000 {
		t.Skip("pre-HF26 already has HF26 percentages — test setup skewed")
	}

	f.HardforksUpTo(hardfork.HF26)
	post := f.DGPO()
	if post.HBDStartPercent != 2000 {
		t.Errorf("HF26 HBDStartPercent: got %d, want 2000", post.HBDStartPercent)
	}
	if post.HBDStopPercent != 2000 {
		t.Errorf("HF26 HBDStopPercent: got %d, want 2000", post.HBDStopPercent)
	}
}

// TestHF21CreatesTreasuryAccount ports the spirit of HF21 activation: the
// "steem.dao" treasury account must exist after HF21 fires.
func TestHF21CreatesTreasuryAccount(t *testing.T) {
	f := New(t)
	// Pre-HF21 — treasury should NOT exist.
	if has, _ := objects.HasAccount(state.NewSession(f.State), "steem.dao"); has {
		t.Skip("steem.dao seeded at genesis in this fixture — test setup skewed")
	}
	f.HardforksUpTo(hardfork.HF21)
	if has, _ := objects.HasAccount(state.NewSession(f.State), "steem.dao"); !has {
		t.Error("expected steem.dao treasury account to exist post-HF21")
	}
}

// TestHF14HBDPrintRateInitialized ports the HF14 portion of
// hardfork-related state setup. HF14 initializes HBD print rate machinery.
func TestHF14HBDPrintRateInitialized(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF14)

	dgpo := f.DGPO()
	if dgpo.HBDStartPercent != 200 {
		t.Errorf("HF14 HBDStartPercent: got %d, want 200 (2%%)", dgpo.HBDStartPercent)
	}
	if dgpo.HBDStopPercent != 500 {
		t.Errorf("HF14 HBDStopPercent: got %d, want 500 (5%%)", dgpo.HBDStopPercent)
	}
	if dgpo.HBDPrintRate != 10000 {
		t.Errorf("HF14 HBDPrintRate: got %d, want 10000 (100%%)", dgpo.HBDPrintRate)
	}
}

// TestHF20IncreasesAccountCreationFee ports a slice of HF20 activation:
// the account_creation_fee is multiplied by
// HIVE_CREATE_ACCOUNT_WITH_HIVE_MODIFIER (= 30) — but ONLY for
// witnesses in wso.current_shuffled_witnesses
// (database_hardfork.cpp:343-353). Witnesses outside the shuffle keep
// their 1× props until they next publish.
func TestHF20IncreasesAccountCreationFee(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF19)

	// alice registers a witness (fee = 1000) but is NOT in the current
	// shuffle; initminer (the sole producer) IS.
	f.Actor("alice")
	f.MustApplyOp(witnessUpdateOp("alice"))
	f.MustApplyOp(witnessUpdateOp(InitMiner))
	f.GenerateBlock()

	pre, err := objects.GetWitness(state.NewSession(f.State), "alice")
	if err != nil {
		t.Fatalf("GetWitness: %v", err)
	}
	feePre := pre.AccountCreationFee
	preInit, err := objects.GetWitness(state.NewSession(f.State), InitMiner)
	if err != nil {
		t.Fatalf("GetWitness initminer: %v", err)
	}
	feePreInit := preInit.AccountCreationFee

	// Activate HF20 — the shuffled producer's fee multiplies by 30;
	// alice's stays 1×.
	f.HardforksUpTo(hardfork.HF20)

	post, err := objects.GetWitness(state.NewSession(f.State), "alice")
	if err != nil {
		t.Fatalf("GetWitness post-HF20: %v", err)
	}
	if post.AccountCreationFee != feePre {
		t.Errorf("HF20 non-shuffled witness fee: pre=%d post=%d, expected unchanged",
			feePre, post.AccountCreationFee)
	}
	postInit, err := objects.GetWitness(state.NewSession(f.State), InitMiner)
	if err != nil {
		t.Fatalf("GetWitness initminer post-HF20: %v", err)
	}
	if postInit.AccountCreationFee != feePreInit*30 {
		t.Errorf("HF20 shuffled witness fee: pre=%d post=%d, expected post=pre*30=%d",
			feePreInit, postInit.AccountCreationFee, feePreInit*30)
	}
}
