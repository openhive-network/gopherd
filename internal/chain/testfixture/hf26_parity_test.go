package testfixture

import (
	"bytes"
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
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
// Go's tx codec preserves per-asset pack provenance via
// Symbol.WasLegacyEncoded (see protocol/asset/symbol.go), and
// tx.IsLegacyPack mirrors C++ full_transaction::is_legacy_pack().
// The pre-HF26 gate in database.cpp is:
//
//	if (!has_hardfork(HIVE_HARDFORK_1_26_ENABLE_NEW_SERIALIZATION))
//	  HIVE_ASSERT(full_transaction->is_legacy_pack(), ...);
//
// so the contract this test pins is: a tx that encodes any asset in
// the HF26 (4-byte NAI) form must report IsLegacyPack==false (the
// pre-HF26 gate would reject it), and a tx whose only assets are in
// the legacy 8-byte form — or that contains no assets at all — must
// report IsLegacyPack==true (the gate accepts both pre- and post-HF26).
//
// Round-trip is the bit-identity contract: RepackAsHF26 then
// RepackAsLegacy must reproduce the original legacy wire byte-for-byte.
func TestHF26PackTransactionBasic(t *testing.T) {
	// Helper: build a SignedTransaction containing the given ops and
	// emit its legacy wire form. Mirrors the C++ test's _get_trx +
	// pack_type::legacy push.
	legacyWire := func(ops ...op.Operation) []byte {
		st := tx.SignedTransaction{
			Transaction: tx.Transaction{
				RefBlockNum:    1,
				RefBlockPrefix: 0xdeadbeef,
				Expiration:     1_700_000_000,
				Operations:     op.Ops(ops...),
			},
		}
		return tx.AppendSignedTransaction(nil, &st)
	}

	hiveLegacy := asset.Asset{
		Amount: 66,
		Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true},
	}
	hbdLegacy := asset.Asset{
		Amount: 13456,
		Symbol: asset.Symbol{Num: asset.HbdSymbol, WasLegacyEncoded: true},
	}

	// transfer — asset-bearing. The C++ test pushes it twice (legacy
	// then hf26); pre-HF26 the hf26 push throws transaction_auth_exception.
	t.Run("transfer", func(t *testing.T) {
		wire := legacyWire(op.Transfer{From: "alice", To: "bob", Amount: hiveLegacy})

		// Legacy wire: gate accepts in any era.
		ok, err := tx.IsLegacyPack(wire)
		if err != nil {
			t.Fatalf("IsLegacyPack(legacy): %v", err)
		}
		if !ok {
			t.Fatal("transfer/legacy: IsLegacyPack=false, want true (gate would reject pre-HF26)")
		}

		// Repack as HF26 (4-byte NAI). The gate rejects this pre-HF26
		// with transaction_auth_exception; HF26+ accepts.
		_, hf26, err := tx.RepackAsHF26(wire)
		if err != nil {
			t.Fatalf("RepackAsHF26: %v", err)
		}
		if bytes.Equal(wire, hf26) {
			t.Fatal("transfer/hf26: wire unchanged after repack — asset symbol not re-encoded")
		}
		// Sanity: hf26 wire is shorter by exactly 4 bytes (8-byte
		// legacy symbol → 4-byte NAI).
		if len(wire)-len(hf26) != 4 {
			t.Fatalf("transfer/hf26: size delta = %d, want 4", len(wire)-len(hf26))
		}
		ok, err = tx.IsLegacyPack(hf26)
		if err != nil {
			t.Fatalf("IsLegacyPack(hf26): %v", err)
		}
		if ok {
			t.Fatal("transfer/hf26: IsLegacyPack=true, want false (pre-HF26 gate must reject)")
		}

		// Round-trip: repack-as-legacy must reproduce the original
		// legacy wire byte-for-byte.
		_, back, err := tx.RepackAsLegacy(hf26)
		if err != nil {
			t.Fatalf("RepackAsLegacy: %v", err)
		}
		if !bytes.Equal(wire, back) {
			t.Fatalf("transfer: legacy round-trip mismatch\n got %x\nwant %x", back, wire)
		}
	})

	// comment — no asset. Both packings must report IsLegacyPack=true
	// (the gate accepts pre- and post-HF26) because there's no asset
	// symbol to disagree about.
	t.Run("comment", func(t *testing.T) {
		commentOp := op.Comment{
			Author:         "alice",
			Permlink:       "lemon",
			ParentPermlink: "ipsum",
			Title:          "this is title",
			Body:           "**body**",
			JSONMetadata:   `{"foo":"bar"}`,
		}
		wire := legacyWire(commentOp)

		ok, err := tx.IsLegacyPack(wire)
		if err != nil || !ok {
			t.Fatalf("comment legacy: IsLegacyPack=%v err=%v, want true/nil", ok, err)
		}

		// "hf26 repack" is a no-op for an asset-less op.
		_, hf26, err := tx.RepackAsHF26(wire)
		if err != nil {
			t.Fatalf("RepackAsHF26: %v", err)
		}
		if !bytes.Equal(wire, hf26) {
			t.Fatalf("comment: hf26 repack changed wire, want identical (no assets)")
		}
		ok, err = tx.IsLegacyPack(hf26)
		if err != nil || !ok {
			t.Fatalf("comment hf26: IsLegacyPack=%v err=%v, want true/nil", ok, err)
		}
	})

	// decline_voting_rights — no asset. Symmetric to comment.
	t.Run("decline_voting_rights", func(t *testing.T) {
		wire := legacyWire(op.DeclineVotingRights{Account: "alice", Decline: true})

		ok, err := tx.IsLegacyPack(wire)
		if err != nil || !ok {
			t.Fatalf("decline_voting_rights legacy: IsLegacyPack=%v err=%v, want true/nil", ok, err)
		}

		_, hf26, err := tx.RepackAsHF26(wire)
		if err != nil {
			t.Fatalf("RepackAsHF26: %v", err)
		}
		if !bytes.Equal(wire, hf26) {
			t.Fatalf("decline_voting_rights: hf26 repack changed wire, want identical (no assets)")
		}
		ok, err = tx.IsLegacyPack(hf26)
		if err != nil || !ok {
			t.Fatalf("decline_voting_rights hf26: IsLegacyPack=%v err=%v, want true/nil", ok, err)
		}
	})

	// comment_options — carries max_accepted_payout. Same contract as
	// transfer: hf26 packing must be detectable by IsLegacyPack.
	t.Run("comment_options", func(t *testing.T) {
		wire := legacyWire(op.CommentOptions{
			Author:               "alice",
			Permlink:             "lemon",
			MaxAcceptedPayout:    hbdLegacy,
			PercentHbd:           10000,
			AllowVotes:           true,
			AllowCurationRewards: true,
		})

		ok, err := tx.IsLegacyPack(wire)
		if err != nil || !ok {
			t.Fatalf("comment_options legacy: IsLegacyPack=%v err=%v, want true/nil", ok, err)
		}

		_, hf26, err := tx.RepackAsHF26(wire)
		if err != nil {
			t.Fatalf("RepackAsHF26: %v", err)
		}
		if len(wire)-len(hf26) != 4 {
			t.Fatalf("comment_options/hf26: size delta = %d, want 4", len(wire)-len(hf26))
		}
		ok, err = tx.IsLegacyPack(hf26)
		if err != nil {
			t.Fatalf("IsLegacyPack(hf26): %v", err)
		}
		if ok {
			t.Fatal("comment_options/hf26: IsLegacyPack=true, want false")
		}

		// Round-trip back to legacy.
		_, back, err := tx.RepackAsLegacy(hf26)
		if err != nil {
			t.Fatalf("RepackAsLegacy: %v", err)
		}
		if !bytes.Equal(wire, back) {
			t.Fatalf("comment_options legacy round-trip mismatch\n got %x\nwant %x", back, wire)
		}
	})

	// Mixed transaction: one asset-bearing op + one asset-less op.
	// IsLegacyPack reflects the asset-bearing one only.
	t.Run("mixed", func(t *testing.T) {
		wire := legacyWire(
			op.Transfer{From: "alice", To: "bob", Amount: hiveLegacy},
			op.DeclineVotingRights{Account: "alice", Decline: true},
		)
		ok, err := tx.IsLegacyPack(wire)
		if err != nil || !ok {
			t.Fatalf("mixed legacy: IsLegacyPack=%v err=%v, want true", ok, err)
		}
		_, hf26, err := tx.RepackAsHF26(wire)
		if err != nil {
			t.Fatalf("RepackAsHF26: %v", err)
		}
		ok, err = tx.IsLegacyPack(hf26)
		if err != nil {
			t.Fatalf("IsLegacyPack(hf26): %v", err)
		}
		if ok {
			t.Fatal("mixed hf26: IsLegacyPack=true, want false (transfer's asset is in NAI form)")
		}
	})
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
