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

	"hived-go/internal/protocol/op"
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

// TestAccountRecoveryOwnerHistoryExpires scaffolds the
// "owner authority history expiration" branch of
// operation_tests.cpp::account_recovery — the C++ test rotates owner
// authority several times across HIVE_OWNER_AUTH_RECOVERY_PERIOD
// (30 days) to verify that an owner_authority_history row created
// 30+ days ago is no longer considered "recent" enough to authorize
// a recover_account.
//
// SKIPPED: hived-go's owner_authority_history is captured by
// recover_account_evaluator (see internal/chain/evaluator/recovery.go)
// but the live-sync replay never asserts on the "owner history
// expired" branch and there is no test-side helper to forge
// authority key matches without a signing layer. Once the fixture
// supports authority-key matching, port the full sub-scenario.
func TestAccountRecoveryOwnerHistoryExpires(t *testing.T) {
	t.Skip("MISSING: owner-authority-history expiration sub-scenario " +
		"of account_recovery requires authority-key signing in the " +
		"testfixture (currently signatures are skipped). C++ rotates " +
		"owner keys across 30d to verify that owner_authority_history " +
		"entries older than HIVE_OWNER_AUTH_RECOVERY_PERIOD reject the " +
		"recover_account op.")
}
