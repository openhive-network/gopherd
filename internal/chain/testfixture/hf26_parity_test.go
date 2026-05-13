package testfixture

import (
	"testing"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// getAccountAuthorityFromFixture reads the AccountAuthority row for the
// named account from the fixture's committed state.
func getAccountAuthorityFromFixture(f *Fixture, name string) (objects.AccountAuthority, error) {
	return objects.GetAccountAuthority(state.NewSession(f.State), auth.AccountName(name))
}

// 1:1 parity scaffolds for hive_tes/tests/unit/tests/hf26_tests.cpp.
//
// The C++ suite covers three behaviors:
//   - update_operation        — HF26 widens the owner-update cooldown
//     window from once/hour to twice/hour.
//   - owner_update_limit      — unit-level coverage of the
//     owner_update_limit_mgr::check helper (both pre-HF26 single-bound
//     and HF26+ previous_time/last_time double-bound).
//   - pack_transaction_basic  — exercises pack_type::legacy vs
//     pack_type::hf26 on transfer / comment / comment_options /
//     decline_voting_rights ops, asserting that asset-bearing ops
//     reject the hf26 packing pre-HF26 and accept it post-HF26.
//
// All three lean on consensus-layer machinery that Go does NOT yet
// implement, so the scaffolds below t.Skip() with brief MISSING notes.

// TestHF26OwnerUpdateLimit is the 1:1 parity name for
// hf26_tests.cpp::update_operation. The C++ test creates `bob`, then
// runs three back-to-back account_update ops rotating bob's owner key.
// Pre-HF26 only the FIRST update succeeds (HIVE_OWNER_UPDATE_LIMIT
// gates the next two). At HF26+ TWO consecutive updates succeed and
// the third throws (twice-per-hour cap).
//
// MISSING in Go: the account_update evaluator tracks
// LastOwnerUpdate / PreviousOwnerUpdate on the account row but does
// NOT enforce HIVE_OWNER_UPDATE_LIMIT as a precondition. See
// internal/chain/evaluator/account_update.go:84-96 — owner rotation
// always succeeds; libraries/chain/util/owner_update_limit_mgr.cpp
// has no Go counterpart. Until the evaluator gains a cooldown check
// (both pre- and post-HF26 branches), the success/throw contrast the
// C++ test asserts cannot be reproduced.
func TestHF26OwnerUpdateLimit(t *testing.T) {
	// Port of hf26_tests.cpp::update_operation. The C++ test creates
	// `bob`, then runs three back-to-back account_update ops rotating
	// bob's owner key:
	//   pre-HF26 — only the first succeeds; updates #2 and #3 throw
	//              ("Owner authority can only be updated once an hour.").
	//   HF26+   — updates #1 and #2 succeed; #3 throws ("…twice an hour.").
	//
	// We exercise two independent fixtures (one fast-forwarded to HF25,
	// the other to HF26) so the prev/last_owner_update timestamps are
	// fresh in each scenario.

	t.Run("pre-HF26", func(t *testing.T) {
		f := New(t)
		f.HardforksUpTo(hardfork.HF25)
		f.Actor("bob")
		f.GenerateBlock()

		// First rotation — bob's last_owner_update is genesis (0); the
		// cooldown gate passes trivially.
		k1 := newAuth([33]byte{0x02, 'K', '1'})
		if err := f.ApplyOp(op.AccountUpdate{Account: "bob", Owner: &k1}); err != nil {
			t.Fatalf("first owner update should succeed: %v", err)
		}

		// Second rotation — only a few seconds since last update; gate
		// must fire pre-HF26.
		k2 := newAuth([33]byte{0x02, 'K', '2'})
		if err := f.ApplyOp(op.AccountUpdate{Account: "bob", Owner: &k2}); err == nil {
			t.Fatal("second owner update within 1 hour should fail pre-HF26")
		}

		// Third rotation — still within window; still fails.
		k3 := newAuth([33]byte{0x02, 'K', '3'})
		if err := f.ApplyOp(op.AccountUpdate{Account: "bob", Owner: &k3}); err == nil {
			t.Fatal("third owner update within 1 hour should fail pre-HF26")
		}

		// Owner authority must still match k1 — the failed updates
		// rolled back cleanly.
		authObj, err := getAccountAuthorityFromFixture(f, "bob")
		if err != nil {
			t.Fatalf("get bob authority: %v", err)
		}
		if !authoritiesEqual(authObj.Owner, k1) {
			t.Fatalf("bob's owner authority should still be k1 after failed updates")
		}

		// After waiting > 1 hour, the next rotation should succeed.
		f.SkipSeconds(3700)
		if err := f.ApplyOp(op.AccountUpdate{Account: "bob", Owner: &k2}); err != nil {
			t.Fatalf("owner update after 1-hour wait should succeed pre-HF26: %v", err)
		}
	})

	t.Run("HF26", func(t *testing.T) {
		f := New(t)
		f.HardforksUpTo(hardfork.HF26)
		f.Actor("carol")
		f.GenerateBlock()

		// First rotation — succeeds (last=prev=0 at creation).
		k1 := newAuth([33]byte{0x02, 'C', '1'})
		if err := f.ApplyOp(op.AccountUpdate{Account: "carol", Owner: &k1}); err != nil {
			t.Fatalf("first owner update should succeed: %v", err)
		}

		// Second rotation — last=t1 (~5s ago), prev=0. HF26 widening:
		// previous is more than 1 hour ago (it's 0 → epoch) → pass.
		k2 := newAuth([33]byte{0x02, 'C', '2'})
		if err := f.ApplyOp(op.AccountUpdate{Account: "carol", Owner: &k2}); err != nil {
			t.Fatalf("second owner update should succeed at HF26+: %v", err)
		}

		// Third rotation — last=t2 (~3s ago), prev=t1 (~6s ago). Both
		// within the 1-hour window → fail.
		k3 := newAuth([33]byte{0x02, 'C', '3'})
		if err := f.ApplyOp(op.AccountUpdate{Account: "carol", Owner: &k3}); err == nil {
			t.Fatal("third owner update within twice-per-hour cap should fail at HF26+")
		}

		// Owner authority should be k2 — the last successful rotation.
		authObj, err := getAccountAuthorityFromFixture(f, "carol")
		if err != nil {
			t.Fatalf("get carol authority: %v", err)
		}
		if !authoritiesEqual(authObj.Owner, k2) {
			t.Fatalf("carol's owner authority should be k2 after 2 successes / 1 failure")
		}

		// After waiting > 1 hour the next rotation succeeds again.
		f.SkipSeconds(3700)
		if err := f.ApplyOp(op.AccountUpdate{Account: "carol", Owner: &k3}); err != nil {
			t.Fatalf("owner update after 1-hour wait should succeed at HF26+: %v", err)
		}
	})
}

// authoritiesEqual compares two Authority structs structurally — enough
// for tests, doesn't need the full FC-encoding equality.
func authoritiesEqual(a, b auth.Authority) bool {
	if a.WeightThreshold != b.WeightThreshold {
		return false
	}
	if len(a.KeyAuths) != len(b.KeyAuths) || len(a.AccountAuths) != len(b.AccountAuths) {
		return false
	}
	for i := range a.KeyAuths {
		if a.KeyAuths[i] != b.KeyAuths[i] {
			return false
		}
	}
	for i := range a.AccountAuths {
		if a.AccountAuths[i] != b.AccountAuths[i] {
			return false
		}
	}
	return true
}

// TestHF26PackTransactionBasic is the 1:1 parity name for
// hf26_tests.cpp::pack_transaction_basic. The C++ test pushes the
// same op twice — once with pack_type::legacy, once with
// pack_type::hf26 — and asserts:
//   - transfer (asset-bearing): legacy always OK; hf26 OK only post-HF26,
//     throws transaction_auth_exception pre-HF26.
//   - comment / decline_voting_rights (no assets): both packings OK
//     in both eras.
//   - comment_options (carries max_accepted_payout asset): hf26 packing
//     same as transfer — pre-HF26 throws, post-HF26 OK.
//
// MISSING in Go: there is no pack_type concept on the test fixture.
// The ApplyOp path serializes through one canonical FC encoder and
// the chain-side push_transaction has no
// serialization_mode_controller / pack_type plumbing. The
// transaction_auth_exception the C++ test catches comes specifically
// from a mismatch between the declared pack_type and the asset
// encoding inside the signed-transaction blob; we never sign here.
func TestHF26PackTransactionBasic(t *testing.T) {
	t.Skip("MISSING: pack_type::legacy vs pack_type::hf26 plumbing. Go tx layer has a single canonical FC encoder (see internal/protocol/tx/tx.go) and the fixture bypasses signature verification, so the pre-HF26 'hf26-packed asset rejection' that produces transaction_auth_exception cannot be observed. serialization_mode_controller is not ported.")
}

// TestHF26UpdateOperation is the 1:1 parity name for
// hf26_tests.cpp::update_operation. NOTE — in the C++ file this case
// shares the `update_operation` BOOST_AUTO_TEST_CASE name with the
// owner-update-cooldown probe (the suite has only three cases:
// update_operation, owner_update_limit, pack_transaction_basic).
// Per the task brief, this scaffold names the third slot (HF26
// update_operation framework) explicitly so the parity matrix stays
// 3-wide; in practice the BOOST case named update_operation is the
// owner-cooldown test covered by TestHF26OwnerUpdateLimit above.
//
// MISSING in Go: same cooldown gap as TestHF26OwnerUpdateLimit. If a
// distinct HF26 "update_operation framework" is later defined
// upstream, this scaffold will be the parity slot to populate; for
// now it skips with a forwarding pointer.
func TestHF26UpdateOperation(t *testing.T) {
	t.Skip("MISSING: HIVE_OWNER_UPDATE_LIMIT enforcement (see TestHF26OwnerUpdateLimit). The C++ BOOST_AUTO_TEST_CASE named 'update_operation' in hf26_tests.cpp IS the owner-cooldown test; this scaffold holds the parity slot. Functional duplication of TestHF26OwnerUpdateLimit until/unless a distinct HF26 'update_operation framework' is split out upstream.")
}

// Compile-time touch so unused-import doesn't fire when every test
// body is a Skip. (hardfork and op are part of the standard fixture
// import set for HF26 tests; keep them referenced so future fills
// don't have to re-add imports.)
var (
	_ = hardfork.HF26
	_ = op.TagAccountUpdate
)
