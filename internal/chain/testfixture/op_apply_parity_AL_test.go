// Scaffolding for cases A..L of
// hive_mast/tests/unit/tests/operation_tests.cpp that aren't already
// covered by the dozens of pre-existing op_*_test.go files (validation
// and authorities are exercised under internal/protocol/op; per-op
// apply behaviour is covered in op_*_test.go scattered across the
// package).
//
// Every C++ test case from `account_auth_tests` through
// `limit_order_create_validate` was cross-checked against the existing
// Go test catalogue. The only gaps were:
//
//   - account_recovery (most aspects are split across the existing
//     TestRequestAccountRecovery / TestRecoverAccountConsumesRequest /
//     TestRecoverAccountExpiredRequestRejected, but the "owner
//     authority history expiration" sub-scenario doesn't have a Go
//     equivalent — skipped, see below)
//   - fixture_convert_checks_balance (regression test for issue #1825)
//   - issue_971_vesting_removal (regression test for delegation
//     removal when account_creation_fee changes between create and
//     remove)
//
// All other A..L cases have a TestXyzValidate / TestXyzAuthorities /
// TestXyzApply that ports the corresponding C++ assertions.

package testfixture

import (
	"testing"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// TestFixtureConvertChecksBalance ports
// operation_tests.cpp::fixture_convert_checks_balance — regression
// test for issue #1825. Submitting a convert op for more HBD than
// the owner holds must be rejected and must NOT drive HBDBalance
// negative.
func TestFixtureConvertChecksBalance(t *testing.T) {
	f := New(t)
	f.Actor("dany")
	f.FundLiquidHBD("dany", 5_000) // 5.000 HBD
	f.GenerateBlock()

	preHBD := f.HBDBalance("dany")

	err := f.ApplyOp(op.Convert{
		Owner:     "dany",
		RequestID: 1,
		Amount:    HBD(5_000_000), // 5000.000 HBD — far more than balance
	})
	if err == nil {
		t.Fatal("convert with insufficient HBD: expected error, got nil")
	}
	// Balance must be unchanged (no partial debit).
	if got := f.HBDBalance("dany"); got != preHBD {
		t.Errorf("dany HBD balance after failed convert: got %d, want %d (unchanged)", got, preHBD)
	}
}

// TestIssue971VestingRemoval ports
// operation_tests.cpp::issue_971_vesting_removal — regression test
// for issue #971: changing the account_creation_fee between a
// `delegate_vesting_shares` create and its subsequent removal (set
// to 0) must NOT block the removal. The delegatee's
// received_vesting_shares drops to 0 immediately, the delegator's
// delegated_vesting_shares remains locked until the return matures.
//
// We don't model account_creation_fee changes between the two ops
// (the Go evaluator does not gate delegate_vesting_shares on the
// fee), so this captures only the symptom: a 0-VESTS re-issue must
// succeed and clear the delegatee's received pool.
func TestIssue971VestingRemoval(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundVesting("alice", 1_000_000) // ~1B VESTS-sat at 1:1000
	f.GenerateBlock()

	const initial = int64(10_000_000)
	if err := f.ApplyOp(op.DelegateVestingShares{
		Delegator:     "alice",
		Delegatee:     "bob",
		VestingShares: VESTS(initial),
	}); err != nil {
		t.Fatalf("initial delegate: %v", err)
	}
	if got := f.Account_("alice").DelegatedVestingShares; got != initial {
		t.Fatalf("alice DelegatedVestingShares post-create: got %d, want %d", got, initial)
	}
	if got := f.Account_("bob").ReceivedVestingShares; got != initial {
		t.Fatalf("bob ReceivedVestingShares post-create: got %d, want %d", got, initial)
	}

	// Remove the delegation. The C++ regression was that this used to
	// throw because account_creation_fee had been raised in between.
	// In Go, the evaluator doesn't reference the fee at removal, so
	// the op should simply succeed.
	if err := f.ApplyOp(op.DelegateVestingShares{
		Delegator:     "alice",
		Delegatee:     "bob",
		VestingShares: VESTS(0),
	}); err != nil {
		t.Fatalf("zero-delegate (remove): %v", err)
	}
	if got := f.Account_("bob").ReceivedVestingShares; got != 0 {
		t.Errorf("bob ReceivedVestingShares after remove: got %d, want 0", got)
	}
	// Delegator's locked pool stays high until the return matures.
	if got := f.Account_("alice").DelegatedVestingShares; got != initial {
		t.Errorf("alice DelegatedVestingShares immediately after remove: "+
			"got %d, want %d (still locked, returns after 5d)", got, initial)
	}
}

// TestAccountRecoveryOwnerHistoryExpires ports the "owner authority
// history expiration" branch of operation_tests.cpp::account_recovery —
// once HIVE_OWNER_AUTH_RECOVERY_PERIOD (30 days) has elapsed since the
// owner_authority_history row was recorded, recover_account_evaluator
// can no longer use that row to authorize the recovery. In C++, the
// periodic account_recovery_processing pass (database.cpp:3702-3710)
// removes stale history rows; the matching loop in
// recover_account_evaluator (hive_evaluator.cpp:2609-2620) then sees
// no row and the FC_ASSERT fires. The Go evaluator applies the same
// 30-day age filter inline.
//
// We exercise the apply path directly — sigs are skipped in the
// testfixture, so authority-key signing isn't required to reach the
// history-match assertion. The test:
//
//   1. Rotates bob's owner key — recording the original owner as a
//      history row at time T0.
//   2. Advances head_block_time by 30 days + epsilon (history row is
//      now older than HIVE_OWNER_AUTH_RECOVERY_PERIOD).
//   3. Requests + attempts recover_account using the now-stale
//      original owner as recent_owner_authority. Must be rejected.
func TestAccountRecoveryOwnerHistoryExpires(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()

	// Record bob's current owner — this is what will be stored as the
	// history row's previous_owner_authority once we rotate.
	bobAuthBefore, err := objects.GetAccountAuthority(state.NewSession(f.State), "bob")
	if err != nil {
		t.Fatalf("get bob auth: %v", err)
	}
	staleOwner := bobAuthBefore.Owner

	// Step 1: rotate bob's owner — creates owner_authority_history
	// row with LastValidTime = head_block_time().
	var newOwnerKey [33]byte
	newOwnerKey[0], newOwnerKey[1] = 0x02, 'N'
	newOwnerAuth := auth.Authority{
		WeightThreshold: 1,
		KeyAuths:        []auth.KeyAuth{{Key: newOwnerKey, Weight: 1}},
	}
	if err := f.ApplyOp(op.AccountUpdate{
		Account: "bob",
		Owner:   &newOwnerAuth,
		MemoKey: newOwnerKey,
	}); err != nil {
		t.Fatalf("rotate owner: %v", err)
	}
	if !objects.HasOwnerAuthorityHistoryFor(state.NewSession(f.State), "bob") {
		t.Fatal("expected owner_authority_history row after rotate")
	}

	// Step 2: skip past HIVE_OWNER_AUTH_RECOVERY_PERIOD + 1h. The
	// stored history row's last_valid_time + 30d is now strictly less
	// than head_block_time, so the row is effectively expired.
	const thirtyDaysSec = uint32(60 * 60 * 24 * 30)
	f.SkipSeconds(thirtyDaysSec + 60*60)

	// Step 3a: alice (bob's recovery account) requests recovery.
	// We make NewOwnerAuthority distinct from the stale one so the
	// op doesn't fail on the "new == recent" check (if any).
	var recoveryKey [33]byte
	recoveryKey[0], recoveryKey[1] = 0x02, 'R'
	recoveryNewAuth := auth.Authority{
		WeightThreshold: 1,
		KeyAuths:        []auth.KeyAuth{{Key: recoveryKey, Weight: 1}},
	}
	if err := f.ApplyOp(op.RequestAccountRecovery{
		RecoveryAccount:   "alice",
		AccountToRecover:  "bob",
		NewOwnerAuthority: recoveryNewAuth,
	}); err != nil {
		t.Fatalf("request_account_recovery: %v", err)
	}

	// Step 3b: recover_account using the now-expired history row's
	// authority. Must be rejected — the row is older than
	// HIVE_OWNER_AUTH_RECOVERY_PERIOD.
	err = f.ApplyOp(op.RecoverAccount{
		AccountToRecover:     "bob",
		NewOwnerAuthority:    recoveryNewAuth,
		RecentOwnerAuthority: staleOwner,
	})
	if err == nil {
		t.Fatal("recover_account using >30d-old owner_authority_history " +
			"should be rejected, got nil error")
	}

	// And bob's account_authority_object must NOT have been rotated —
	// the rejected op should be a no-op on the authority side. We
	// confirm the owner is still the post-rotate key, not the recovery
	// key.
	bobAuthAfter, err := objects.GetAccountAuthority(state.NewSession(f.State), "bob")
	if err != nil {
		t.Fatalf("get bob auth after: %v", err)
	}
	if len(bobAuthAfter.Owner.KeyAuths) != 1 ||
		bobAuthAfter.Owner.KeyAuths[0].Key != newOwnerKey {
		t.Errorf("bob.Owner after rejected recover: got %+v, want owner=newOwnerKey",
			bobAuthAfter.Owner)
	}
}
