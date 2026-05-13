package testfixture

// Port of hive_tes/tests/unit/plugin_tests/rc_plugin_tests.cpp into Go.
// One Go test function per BOOST_AUTO_TEST_CASE in the C++ source.
//
// Coverage map (11 C++ cases):
//
//   account_creation                — TestRCPluginAccountCreation (ported,
//                                      reduced: pre-HF13 PoW paths
//                                      stubbed; post-HF20 account_create
//                                      + create_claimed_account ported).
//   rc_usage_buckets                — TestRCPluginUsageBuckets (already
//                                      covered structurally by
//                                      TestRCUsageBucketRotation +
//                                      TestRCUsageBucketsAdvancedStub;
//                                      this is a documenting alias
//                                      pointing at those + skip on the
//                                      per-op cost flavor).
//   rc_single_recover_account       — TestRCPluginSingleRecoverAccount
//                                      (skipped — RC Phase 5).
//   rc_many_recover_accounts        — TestRCPluginManyRecoverAccounts
//                                      (skipped — RC Phase 5).
//   rc_multisig_recover_account     — TestRCPluginMultisigRecoverAccount
//                                      (skipped — RC Phase 5).
//   rc_tx_order_bug                 — TestRCPluginTxOrderBug
//                                      (skipped — RC Phase 5 + pop_block).
//   rc_pending_data_reset           — TestRCPluginPendingDataReset
//                                      (skipped — RC Phase 5; existing
//                                      TestRCPoolPendingDataReset covers
//                                      stats archive, NOT rc_pending_data
//                                      diff/usage which is a Phase 5
//                                      object).
//   rc_differential_usage_operations— TestRCPluginDifferentialUsageOperations
//                                      (skipped — RC Phase 5).
//   rc_differential_usage_negative  — TestRCPluginDifferentialUsageNegative
//                                      (skipped — RC Phase 5).
//   rc_differential_usage_many_ops  — TestRCPluginDifferentialUsageManyOps
//                                      (skipped — RC Phase 5).
//   rc_exception_during_modify      — TestRCPluginExceptionDuringModify
//                                      (skipped — RC Phase 5 not_enough_rc
//                                      exception path).
//
// Why so many skips: Phase 4 (resource_param + set_slot_delegator) and
// Phase 5 (per-tx RC cost deduction in the evaluator pipeline) are still
// deferred per the RC subsystem roadmap. Every C++ case here that
// exercises the actual rc-charging path (single/many/multisig recover,
// tx_order_bug, differential_usage_*, exception_during_modify) needs
// that machinery to fire. They're scaffolded as skips so the test names
// line up 1:1 with the C++ suite; the bodies become real assertions
// once Phase 5 lands.

import (
	"testing"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// TestRCPluginAccountCreation mirrors rc_plugin_tests.cpp::account_creation.
//
// The C++ test creates 5 actors at successive hardforks via every
// account-creation pathway (pow, pow2, account_create_with_delegation,
// account_create, create_claimed_account) and asserts an
// `rc_account_object` exists for each name after the block. Our port
// drops the rc_account_object indirection — RC fields live directly on
// objects.Account in this Go port — so the assertion becomes
// `f.HasAccount(name)` post-creation.
//
// Pre-HF13 PoW paths and the HF13-only pow2 path are skipped because
// the Go fixture deliberately bypasses the on-chain
// pow/pow2 path (it writes accounts via direct PutAccount, like
// hived's `account_create` debug helper). The post-HF17
// account_create_with_delegation, post-HF20 account_create, and
// post-HF24 create_claimed_account paths run through their real
// evaluators here.
func TestRCPluginAccountCreation(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)

	// initminer needs balance + vests for the fee/delegation.
	f.FundLiquid(InitMiner, 1_000_000)
	f.FundVesting(InitMiner, 1_000_000)
	f.GenerateBlock()

	pubKey := [33]byte{0x02, 'A'}

	// post-HF17 account_create_with_delegation path → "sam".
	if err := f.ApplyOp(op.AccountCreateWithDelegation{
		Fee:            HIVE(0),
		Delegation:     VESTS(100_000_000),
		Creator:        InitMiner,
		NewAccountName: "sam",
		Owner:          newAuth(pubKey),
		Active:         newAuth(pubKey),
		Posting:        newAuth(pubKey),
		MemoKey:        pubKey,
	}); err != nil {
		t.Fatalf("account_create_with_delegation(sam): %v", err)
	}
	if !f.HasAccount("sam") {
		t.Fatalf("expected account 'sam' to exist post-create_with_delegation")
	}

	// post-HF20 vanilla account_create path → "dave".
	f.HardforksUpTo(hardfork.HF20)
	if err := f.ApplyOp(op.AccountCreate{
		Fee:            HIVE(0),
		Creator:        InitMiner,
		NewAccountName: "dave",
		Owner:          newAuth(pubKey),
		Active:         newAuth(pubKey),
		Posting:        newAuth(pubKey),
		MemoKey:        pubKey,
	}); err != nil {
		t.Fatalf("account_create(dave): %v", err)
	}
	if !f.HasAccount("dave") {
		t.Fatalf("expected account 'dave' to exist post-account_create")
	}

	// post-HF20 create_claimed_account path → "greg". This requires
	// bumping initminer's pending_claimed_accounts first, exactly as
	// the C++ test does via debug_update.
	{
		// Bump pending_claimed_accounts on initminer (mirrors the
		// C++ debug_update step at line ~219 in account_creation).
		bumpPendingClaimedAccounts(t, f, InitMiner, 1)

		if err := f.ApplyOp(op.CreateClaimedAccount{
			Creator:        InitMiner,
			NewAccountName: "greg",
			Owner:          newAuth(pubKey),
			Active:         newAuth(pubKey),
			Posting:        newAuth(pubKey),
			MemoKey:        pubKey,
		}); err != nil {
			t.Fatalf("create_claimed_account(greg): %v", err)
		}
		if !f.HasAccount("greg") {
			t.Fatalf("expected account 'greg' to exist post-create_claimed_account")
		}
	}

	// The post-HF20 RC-init hook runs the manabar on each newly
	// created account; we already validate that path in
	// TestRCManabarPostHF20InitOnCreate. Here we just spot-check
	// that the recent creates left the manabar field set.
	dave := f.Account_("dave")
	if dave.RCManaLastUpdate == 0 {
		t.Fatalf("dave.RCManaLastUpdate should be initialized post-HF20")
	}
	greg := f.Account_("greg")
	if greg.RCManaLastUpdate == 0 {
		t.Fatalf("greg.RCManaLastUpdate should be initialized post-HF20")
	}
}

// bumpPendingClaimedAccounts adds n to the named account's
// PendingClaimedAccounts via direct state mutation. Mirrors the
// db_plugin->debug_update step in the C++ account_creation case for
// the create_claimed_account leg.
func bumpPendingClaimedAccounts(t *testing.T, f *Fixture, name string, n uint32) {
	t.Helper()
	s := state.NewSession(f.State)
	a, err := objects.GetAccount(s, name)
	if err != nil {
		t.Fatalf("bumpPendingClaimedAccounts: get %q: %v", name, err)
	}
	a.PendingClaimedAccounts += uint16(n)
	objects.PutAccount(s, &a)
	if err := s.Commit(false); err != nil {
		t.Fatalf("bumpPendingClaimedAccounts: commit: %v", err)
	}
}

// TestRCPluginUsageBuckets mirrors rc_plugin_tests.cpp::rc_usage_buckets.
//
// The C++ case exercises 24-hour rolling usage buckets across many
// transactions and asserts the per-resource consumption distributes
// into the right bucket as time advances. The structural rotation half
// of that case is already covered by TestRCUsageBucketRotation (basic
// rotation across the 3600s boundary) and TestRCUsageBucketsAdvancedStub
// (the per-op cost-accounting flavor). This function exists as a
// 1:1-named alias to keep the C++/Go cross-reference clean.
func TestRCPluginUsageBuckets(t *testing.T) {
	t.Skip("RC Phase 5: per-op cost accumulation into hourly buckets requires per-tx cost deduction; structural rotation covered by TestRCUsageBucketRotation")
}

// TestRCPluginSingleRecoverAccount mirrors rc_plugin_tests.cpp::
// rc_single_recover_account — recover_account is RC-free standalone but
// charged when bundled with paid ops. Needs Phase 5 to charge mana.
func TestRCPluginSingleRecoverAccount(t *testing.T) {
	t.Skip("RC Phase 5: rc_single_recover_account requires per-tx RC cost deduction (has_mana plugin_exception path)")
}

// TestRCPluginManyRecoverAccounts mirrors rc_plugin_tests.cpp::
// rc_many_recover_accounts — multiple recover_account ops in one tx are
// disallowed at the subsidized layer. Needs Phase 5.
func TestRCPluginManyRecoverAccounts(t *testing.T) {
	t.Skip("RC Phase 5: rc_many_recover_accounts requires per-tx RC cost deduction + single-subsidized-op rule")
}

// TestRCPluginMultisigRecoverAccount mirrors rc_plugin_tests.cpp::
// rc_multisig_recover_account — auth-complexity-based RC discount path
// (only 2 signatures allowed for subsidized recovery). Needs Phase 5.
func TestRCPluginMultisigRecoverAccount(t *testing.T) {
	t.Skip("RC Phase 5: rc_multisig_recover_account requires per-tx RC cost deduction + signature-count check")
}

// TestRCPluginTxOrderBug mirrors rc_plugin_tests.cpp::rc_tx_order_bug —
// pop_block + reapply pending transaction interaction with the RC
// plugin. Needs Phase 5 + pop_block support in the fixture.
func TestRCPluginTxOrderBug(t *testing.T) {
	t.Skip("RC Phase 5 + pop_block: rc_tx_order_bug requires per-tx RC cost deduction + block pop/reapply")
}

// TestRCPluginPendingDataReset mirrors rc_plugin_tests.cpp::
// rc_pending_data_reset.
//
// The C++ test pokes at db.get<rc_pending_data>() — a singleton that
// accumulates per-tx pending_usage / pending_cost / differential_usage
// across the block and resets at block boundary. The Go port has no
// rc_pending_data object yet; that singleton is part of Phase 5's per-tx
// cost-deduction infrastructure. The existing
// TestRCPoolPendingDataReset covers the rc_stats archive boundary,
// which is a DIFFERENT object.
func TestRCPluginPendingDataReset(t *testing.T) {
	t.Skip("RC Phase 5: rc_pending_data_reset requires rc_pending_data singleton + per-op differential usage tracking")
}

// TestRCPluginDifferentialUsageOperations mirrors rc_plugin_tests.cpp::
// rc_differential_usage_operations — per-op differential state_bytes
// usage on account_update / witness_update / etc. Needs Phase 5.
func TestRCPluginDifferentialUsageOperations(t *testing.T) {
	t.Skip("RC Phase 5: rc_differential_usage_operations requires per-op differential usage tracking")
}

// TestRCPluginDifferentialUsageNegative mirrors rc_plugin_tests.cpp::
// rc_differential_usage_negative — verifies usage can go negative when
// an op shrinks state. Needs Phase 5.
func TestRCPluginDifferentialUsageNegative(t *testing.T) {
	t.Skip("RC Phase 5: rc_differential_usage_negative requires signed differential usage accumulator")
}

// TestRCPluginDifferentialUsageManyOps mirrors rc_plugin_tests.cpp::
// rc_differential_usage_many_ops — multi-op transaction differential
// usage must run per-op pre-apply, not per-tx. Needs Phase 5.
func TestRCPluginDifferentialUsageManyOps(t *testing.T) {
	t.Skip("RC Phase 5: rc_differential_usage_many_ops requires per-op (not per-tx) differential usage visitor")
}

// TestRCPluginExceptionDuringModify mirrors rc_plugin_tests.cpp::
// rc_exception_during_modify — verifies the chain throws
// not_enough_rc_exception with a populated tx_info.payer when an
// account's mana is insufficient. Needs Phase 5.
func TestRCPluginExceptionDuringModify(t *testing.T) {
	t.Skip("RC Phase 5: rc_exception_during_modify requires not_enough_rc_exception + tx_info payload")
}
