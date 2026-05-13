package testfixture

import (
	"testing"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Parity scaffold for /home/martin/projects/hive_tes/tests/unit/
// plugin_tests/direct_rc_delegation.cpp — all 13 BOOST_AUTO_TEST_CASE
// rows enumerated here. The Go port lands these progressively as the
// RC subsystem fills in:
//
//   Covered today (op-level evaluator behaviour, Phase 3):
//     - delegate_rc_operation_validate
//         → TestDelegateRCOperationValidate          (op_delegate_rc_test.go)
//     - delegate_rc_operation_apply_single
//         → TestDelegateRCOperationApplySingle       (op_delegate_rc_test.go)
//     - delegate_rc_operation_apply_many
//         → TestDelegateRCOperationApplyMany         (op_delegate_rc_test.go)
//     - delegate_rc_operation_apply_many_different
//         → TestDelegateRCOperationApplyManyDifferent (op_delegate_rc_test.go)
//     - rc_delegation_removal_no_rc
//         → TestRCDelegationRemovalNoRC              (op_delegate_rc_test.go)
//
//   Covered today (Phase 2 structural — clip + expired-row queue):
//     - update_outdel_overflow
//         → TestRCUpdateOutdelOverflow               (op_rc_phase2_test.go)
//     - update_outdel_overflow_many_accounts
//         → TestRCUpdateOutdelOverflowManyAccounts   (op_rc_phase2_test.go)
//
//   Ported here today (manabar arithmetic — no Phase 5 dependency):
//     - rc_delegation_regeneration
//         → TestDirectRCDelegationRegeneration       (this file)
//     - rc_negative_regeneration_bug
//         → TestDirectRCNegativeRegenerationBug      (this file)
//
//   Skipped pending later phases:
//     - direct_rc_delegation_vesting_withdrawal
//         → t.Skip("MISSING: handleVestingWithdrawalRCImpact not wired
//           into processOneWithdrawal — needs Phase 4 + creation_rc")
//     - direct_rc_delegation_vesting_withdrawal_routes
//         → same plus set_withdraw_vesting_route auto-vest interaction
//     - update_outdel_overflow_delegatee
//         → t.Skip("MISSING: RC Phase 5 cost deduction + creation_rc")
//     - update_outdel_overflow_delegatee_performance
//         → t.Skip("MISSING: RC Phase 5 cost deduction (perf-only)")
//
// The "ported today" pair below exercises the RC manabar regeneration
// model the C++ test asserts on: more delegated_rc → larger max_rc →
// the same dt of regeneration produces a strictly larger delta. We
// poke the manabar state directly (matching the C++ debug-update hook
// that artificially sets current_mana / last_update_time), then call
// the same RegenerateRCMana the chain calls and compare deltas.

// TestDirectRCDelegationRegeneration ports
// direct_rc_delegation.cpp::rc_delegation_regeneration. The C++ test
// drains bob's manabar via 100+ tiny transfers in four phases —
// (1) baseline no delegation, (2) full delegation, (3) half
// delegation, (4) delegation removed — and checks the per-window
// regeneration delta strictly increases with max_rc.
//
// Without Phase 5 we cannot drain mana via transactions, but we can
// pin current_mana at a known low value and observe the regen delta
// for a fixed dt; that is the property the original test asserts.
func TestDirectRCDelegationRegeneration(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF26)
	f.Actors("alice", "bob")
	f.FundVesting("alice", 10_000)
	f.GenerateBlock()

	// Helper: pin bob's RC manabar at a low value at time T0, then
	// regenerate to T0+dt and report the delta. Mirrors the C++
	// regenerate_mana(get_time() + 1) probe.
	regenDelta := func(label string, pinTime uint32, dt uint32) int64 {
		s := state.NewSession(f.State)
		bob, _ := objects.GetAccount(s, "bob")
		// Pin: drain to zero at pinTime so dt of regen is well-defined.
		bob.RCManaCurrent = 0
		bob.RCManaLastUpdate = pinTime
		objects.PutAccount(s, &bob)
		if err := s.Commit(false); err != nil {
			t.Fatalf("%s: pin: %v", label, err)
		}
		s2 := state.NewSession(f.State)
		bob2, _ := objects.GetAccount(s2, "bob")
		objects.RegenerateRCMana(&bob2, pinTime+dt)
		return bob2.RCManaCurrent
	}

	now := f.HeadBlockTime()
	const probeDt uint32 = 60 // 60s of regen — tiny slice of the 5-day window

	deltaNoDelegation := regenDelta("baseline", now, probeDt)

	// (2) alice delegates ALL her vests to bob. bob's max_rc jumps.
	aliceVests := f.Vests("alice")
	if err := f.ApplyOp(rcDelegationCustomOp("alice", []string{"bob"}, aliceVests)); err != nil {
		t.Fatalf("delegate full: %v", err)
	}
	deltaFullDelegation := regenDelta("full-delegation", now, probeDt)
	if deltaFullDelegation <= deltaNoDelegation {
		t.Fatalf("full > none: got %d vs %d", deltaFullDelegation, deltaNoDelegation)
	}

	// (3) Reduce to half.
	if err := f.ApplyOp(rcDelegationCustomOp("alice", []string{"bob"}, aliceVests/2)); err != nil {
		t.Fatalf("delegate half: %v", err)
	}
	deltaHalfDelegation := regenDelta("half-delegation", now, probeDt)
	if deltaFullDelegation <= deltaHalfDelegation {
		t.Fatalf("full > half: got full=%d half=%d", deltaFullDelegation, deltaHalfDelegation)
	}
	if deltaHalfDelegation <= deltaNoDelegation {
		t.Fatalf("half > none: got half=%d none=%d", deltaHalfDelegation, deltaNoDelegation)
	}

	// (4) Remove the delegation. Regen should collapse back to the
	// baseline (creation_rc only; in our schema, max_rc reverts to
	// bob's intrinsic effective_vests = 0 → no delta).
	if err := f.ApplyOp(rcDelegationCustomOp("alice", []string{"bob"}, 0)); err != nil {
		t.Fatalf("delegate remove: %v", err)
	}
	deltaRemoved := regenDelta("removed", now, probeDt)
	if deltaFullDelegation <= deltaRemoved {
		t.Fatalf("full > removed: got full=%d removed=%d", deltaFullDelegation, deltaRemoved)
	}
	if deltaHalfDelegation <= deltaRemoved {
		t.Fatalf("half > removed: got half=%d removed=%d", deltaHalfDelegation, deltaRemoved)
	}
	// Equal-baseline check: C++ asserts removed == no_delegation. Our
	// bob has effective_vests=0 throughout, so both are 0.
	if deltaRemoved != deltaNoDelegation {
		t.Fatalf("removed == none: got removed=%d none=%d", deltaRemoved, deltaNoDelegation)
	}
}

// TestDirectRCNegativeRegenerationBug ports the gist of
// direct_rc_delegation.cpp::rc_negative_regeneration_bug. The C++ test
// reproduces a historical bug where a delegatee whose RC mana went
// negative (artificially, in test) and then lost their delegation
// would, after a long quiet period, end up with *less* RC than an
// otherwise-identical control account that had its regen triggered
// continuously.
//
// The fix: when a delegation drops, regenerate the delegatee's mana
// FIRST against the OLD max_rc, THEN subtract the dropped delegation.
// That way the dt accumulated under the higher max regenerates at the
// higher rate, instead of being "lost" once the cap shrinks.
//
// We can verify the property without Phase 5: pin a delegatee at
// negative mana with stale last_update_time, drop their delegation,
// and check current_mana is at least as high as a parallel control
// account that never had its delegation dropped.
func TestDirectRCNegativeRegenerationBug(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF26)
	f.Actors("delegator", "delegatee", "control")
	f.FundVesting("delegator", 10_000)
	f.GenerateBlock()

	delegatorVests := f.Vests("delegator")

	// Two parallel delegations of the same size: one to delegatee,
	// one to control. Both start with identical RC manabars.
	if err := f.ApplyOp(rcDelegationCustomOp("delegator", []string{"delegatee"}, delegatorVests/3)); err != nil {
		t.Fatalf("delegate→delegatee: %v", err)
	}
	if err := f.ApplyOp(rcDelegationCustomOp("delegator", []string{"control"}, delegatorVests/3)); err != nil {
		t.Fatalf("delegate→control: %v", err)
	}

	// Artificially poke both delegatees into deep-negative mana with
	// a stale timestamp (one full regen window ago) — mirrors the
	// HIVE_RC_MAX_NEGATIVE_PERCENT scenario.
	s := state.NewSession(f.State)
	now := f.HeadBlockTime()
	stale := now - objects.RCRegenTimeSec
	for _, name := range []string{"delegatee", "control"} {
		a, _ := objects.GetAccount(s, name)
		full := a.MaximumRC()
		// HIVE_RC_MAX_NEGATIVE_PERCENT = 10% of max; sign-flip.
		a.RCManaCurrent = -(full / 10)
		a.RCManaLastUpdate = stale
		objects.PutAccount(s, &a)
	}
	if err := s.Commit(false); err != nil {
		t.Fatalf("poke: %v", err)
	}

	// Drop delegatee's delegation but NOT control's. The C++ regression
	// is that delegatee's mana ends up lower than control's because the
	// pre-drop regen window wasn't credited at the higher cap.
	if err := f.ApplyOp(rcDelegationCustomOp("delegator", []string{"delegatee"}, 0)); err != nil {
		t.Fatalf("drop delegatee: %v", err)
	}

	// Refresh both manabars at the same wall-clock time.
	now2 := f.HeadBlockTime() + 1
	s = state.NewSession(f.State)
	deleg, _ := objects.GetAccount(s, "delegatee")
	ctrl, _ := objects.GetAccount(s, "control")
	objects.RegenerateRCMana(&deleg, now2)
	objects.RegenerateRCMana(&ctrl, now2)

	// After the regen+drop, delegatee should never fall further behind
	// control by *more than* the delegation amount that was dropped
	// (delegatorVests/3). The "negative regen bug" manifested as a
	// deficit larger than the dropped delegation.
	delta := ctrl.RCManaCurrent - deleg.RCManaCurrent
	maxLegal := delegatorVests/3 + 1
	if delta > maxLegal {
		t.Fatalf("delegatee deficit %d exceeds dropped delegation %d — bug present",
			delta, maxLegal)
	}
}

// TestDirectRCDelegationVestingWithdrawal scaffolds the C++
// direct_rc_delegation_vesting_withdrawal case. The integration path
// needs handleVestingWithdrawalRCImpact wired into the per-chunk
// vesting-withdrawal codepath (rc_phase2.go has the function exported
// but processOneWithdrawal currently only calls RegenerateRCMana
// without the delegation-clip pass).
//
// The structural shape is already covered by TestRCUpdateOutdelOverflow
// in op_rc_phase2_test.go (which invokes the hook directly via the
// test shim). The exact-numbered, multi-week walk in the C++ test
// requires:
//
//   1. Wiring handleVestingWithdrawalRCImpact into processOneWithdrawal
//      (Phase 4 — chain-integration step).
//   2. Phase 5 per-tx RC cost deduction so the exact equality on
//      current_mana = creation_rc + N - cost(op) holds.
func TestDirectRCDelegationVestingWithdrawal(t *testing.T) {
	t.Skip("MISSING: RC Phase 4 (wire handleVestingWithdrawalRCImpact into processOneWithdrawal) + Phase 5 (per-tx cost)")
}

// TestDirectRCDelegationVestingWithdrawalRoutes scaffolds the
// withdraw_routes variant — same Phase 4/5 gap as above, plus the
// auto_vest=true route path must propagate its VESTS deposit through
// the recipient's max_rc before the per-chunk clip runs.
func TestDirectRCDelegationVestingWithdrawalRoutes(t *testing.T) {
	t.Skip("MISSING: RC Phase 4 + Phase 5 (see TestDirectRCDelegationVestingWithdrawal)")
}

// TestUpdateOutdelOverflowDelegatee scaffolds the case where the
// delegation chain is (alice → bob via delegate_vesting_shares) and
// then bob redelegates RC to dave+eve. Removing alice's vesting
// delegation cascades: bob's effective_vests drops below his
// out-delegated RC, triggering the overflow clip on bob's row.
//
// Needs Phase 4 (chained vesting/delegate_vesting → RC impact hook)
// + Phase 5 (cost-precise asserts on the creation_rc adjustments).
func TestUpdateOutdelOverflowDelegatee(t *testing.T) {
	t.Skip("MISSING: RC Phase 4 + Phase 5 — cascaded vesting/RC clip on delegatee")
}

// TestUpdateOutdelOverflowDelegateePerformance scaffolds the 100k-
// account perf variant. Same Phase 4/5 prerequisite; even with those,
// the C++ test asserts a wall-clock budget that needs the rc_account
// secondary index in shape — skip until the subsystem fills out.
func TestUpdateOutdelOverflowDelegateePerformance(t *testing.T) {
	t.Skip("MISSING: RC Phase 4 + Phase 5 (perf variant of TestUpdateOutdelOverflowDelegatee)")
}
