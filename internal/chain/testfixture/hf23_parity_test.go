package testfixture

import "testing"

// 1:1 parity scaffolds for hive_mast/tests/unit/tests/hf23_tests.cpp.
//
// Most cases exercise db->clear_account() / db->restore_accounts() /
// db->gather_balance() — the Steemit-takeover-era drain & restore hooks
// fired at HF23/HF24. In Go those are chain.clearAccounts and
// chain.restoreAccounts, both package-private and (for restore) an
// intentional no-op because we don't snapshot pre-clear balances.
// See internal/chain/hardfork_accounts.go.
//
// Each Go test scaffold below mirrors a C++ BOOST_AUTO_TEST_CASE 1:1.
// Cross-references point at the place where coverage already lives
// (operation_time_test.go combined skip stubs, hf23_extra_test.go
// HF23-flavor port, hf_test.go::TestHF23AirdropClears) so the parity
// matrix stays explicit even when the live behaviour is a Skip.

// --- basic_test_01..06 — all six exercise clear_account on various
// VEST/delegation setups. Coverage already lives in
// operation_time_test.go::TestHF23BasicTest01 (combined skip stub).

func TestHF23BasicTest02(t *testing.T) {
	t.Skip("MISSING: HF23 clear_account() public hook — basic_test_02 (vesting; clear alice then bob) requires chain.clearAccounts to be callable from tests (it's package-private). Covered as combined skip in operation_time_test.go::TestHF23BasicTest01.")
}

func TestHF23BasicTest03(t *testing.T) {
	t.Skip("MISSING: HF23 clear_account() public hook — basic_test_03 (vest-to-others matrix) requires chain.clearAccounts to be callable from tests. Covered as combined skip in operation_time_test.go::TestHF23BasicTest01.")
}

func TestHF23BasicTest04(t *testing.T) {
	t.Skip("MISSING: HF23 clear_account() public hook — basic_test_04 (delegate_vest + clear sender drops delegations) requires chain.clearAccounts to be callable from tests. Covered as combined skip in operation_time_test.go::TestHF23BasicTest01.")
}

func TestHF23BasicTest05(t *testing.T) {
	t.Skip("MISSING: HF23 clear_account() public hook — basic_test_05 (delegate_vest receiver/giver matrix with clear) requires chain.clearAccounts to be callable from tests. Covered as combined skip in operation_time_test.go::TestHF23BasicTest01.")
}

func TestHF23BasicTest06(t *testing.T) {
	t.Skip("MISSING: HF23 clear_account() public hook — basic_test_06 (vesting_delegation_expiration_object generation on clear) requires chain.clearAccounts to be callable from tests. Covered as combined skip in operation_time_test.go::TestHF23BasicTest01.")
}

// --- save_test_01, save_test_02 — exercise db->gather_balance() and
// inspect hardfork_property_object.h23_balances. No h23_balances
// snapshot is taken in Go (restoreAccounts is a no-op, see
// internal/chain/hardfork_accounts.go:102). Covered as combined skip
// in operation_time_test.go::TestHF23SaveTest.

func TestHF23SaveTest01(t *testing.T) {
	t.Skip("MISSING: HF23 gather_balance / h23_balances snapshot — save_test_01 (snapshot multiple accounts) requires chain.restoreAccounts to capture pre-clear balances (intentional no-op today). Covered as combined skip in operation_time_test.go::TestHF23SaveTest.")
}

func TestHF23SaveTest02(t *testing.T) {
	t.Skip("MISSING: HF23 gather_balance / h23_balances snapshot — save_test_02 (snapshot one account) requires the same. Covered as combined skip in operation_time_test.go::TestHF23SaveTest.")
}

// --- restore_accounts_01, restore_accounts_02 — exercise
// db->restore_accounts() against a pre-HF24 snapshot. Go's
// chain.restoreAccounts is an intentional no-op (no snapshot is
// taken — Pebble's undo log doesn't keep history that far back).
// Covered as combined skip in operation_time_test.go::TestHF23RestoreAccounts.

func TestHF23RestoreAccounts01(t *testing.T) {
	t.Skip("MISSING: HF23 restore_accounts mechanism — restore_accounts_01 (treasury → restored accounts re-credit) needs a pre-clear h23_balances snapshot; restoreAccounts is a no-op in Go (see internal/chain/hardfork_accounts.go:102). Covered as combined skip in operation_time_test.go::TestHF23RestoreAccounts.")
}

func TestHF23RestoreAccounts02(t *testing.T) {
	t.Skip("MISSING: HF23 restore_accounts mechanism — restore_accounts_02 (snapshot during HF23, restore during HF24) needs the same. Covered as combined skip in operation_time_test.go::TestHF23RestoreAccounts.")
}

// --- escrow_cleanup_test, limit_order_cleanup_test,
// convert_request_cleanup_test — all three exercise clear_account
// against active escrow / limit_order / convert_request rows, asserting
// the cleanup walks every secondary index. In Go clearAccounts only
// touches the Account row (balances + savings + vests + reward bucket);
// active escrows / orders / convert requests are NOT swept. See
// internal/chain/hardfork_accounts.go:22 — secondary-index cleanup is
// not implemented because the HF23 mainnet account list (which is the
// only caller) had been drained of these states ahead of time.

func TestHF23EscrowCleanupTest(t *testing.T) {
	t.Skip("MISSING: HF23 clear_account escrow sweep — escrow_cleanup_test requires chain.clearAccounts to walk the escrow_index by from and refund/redirect every pending escrow (with pending_transfers decrement). The Go port only zeroes balance fields; secondary indices are untouched. See internal/chain/hardfork_accounts.go:22.")
}

func TestHF23LimitOrderCleanupTest(t *testing.T) {
	t.Skip("MISSING: HF23 clear_account limit-order sweep — limit_order_cleanup_test requires chain.clearAccounts to walk the limit_order_index by seller and cancel each open order (returning the locked balance to the treasury). The Go port only zeroes balance fields. See internal/chain/hardfork_accounts.go:22.")
}

func TestHF23ConvertRequestCleanupTest(t *testing.T) {
	t.Skip("MISSING: HF23 clear_account convert-request sweep — convert_request_cleanup_test requires chain.clearAccounts to walk convert_request_index by owner. The Go port only zeroes balance fields; the C++ test additionally asserts that an open collateralized_convert_request triggers an assert (clear_account refuses to drop the account) — neither path is implemented. See internal/chain/hardfork_accounts.go:22.")
}

// --- hbd_test_01, hbd_test_02 — clear_account with HBD interest accrual.
// Covered as TestHF23HBDTest01 / TestHF23HBDTest02 in
// operation_time_test.go. Mirror-named scaffolds kept here for parity.

func TestHF23HBDTest01Parity(t *testing.T) {
	t.Skip("MISSING: HF23 clear_account() public hook — hbd_test_01 (clear with zero HBD, then with funded HBD). Covered as skip in operation_time_test.go::TestHF23HBDTest01.")
}

func TestHF23HBDTest02Parity(t *testing.T) {
	t.Skip("MISSING: HF23 clear_account() public hook + interest_operation virtual-op trail — hbd_test_02 (clear after HBD interest compound). Covered as skip in operation_time_test.go::TestHF23HBDTest02.")
}

// --- savings_test_01, savings_test_02 — clear_account zeroes
// transfer-to-savings HBD balances. Covered as
// operation_time_test.go::TestHF23SavingsTest01/02. Mirror-named
// scaffolds kept here for parity.

func TestHF23SavingsTest01Parity(t *testing.T) {
	t.Skip("MISSING: HF23 clear_account() public hook — savings_test_01 (transfer_to_savings then clear → savings zeroed). Covered as skip in operation_time_test.go::TestHF23SavingsTest01.")
}

func TestHF23SavingsTest02Parity(t *testing.T) {
	t.Skip("MISSING: HF23 clear_account() public hook — savings_test_02 (transfer_to_savings, interest compound, then clear). Covered as skip in operation_time_test.go::TestHF23SavingsTest02.")
}
