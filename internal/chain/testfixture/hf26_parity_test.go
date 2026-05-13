package testfixture

import (
	"testing"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
)

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
	t.Skip("MISSING: HIVE_OWNER_UPDATE_LIMIT cooldown enforcement in account_update evaluator — LastOwnerUpdate/PreviousOwnerUpdate are tracked but never checked. owner_update_limit_mgr::check has no Go port. C++ test asserts 1 success / 2 throws pre-HF26 and 2 successes / 1 throw post-HF26.")
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
