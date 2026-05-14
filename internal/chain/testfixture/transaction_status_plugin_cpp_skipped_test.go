package testfixture

import "testing"

// Skipped ports from hive/tests/unit/plugin_tests/transaction_status.cpp.
//
// The C++ suite has one case (transaction_status_test) that drives
// the transaction_status_plugin: a configurable nominal-depth
// tx-status lookup index activated post-genesis. The plugin tracks
// recently-included transactions and answers "did tx X make it into
// a block, and how deep is it?" queries.
//
// The Go port has no transaction_status_plugin, so the test has
// nothing to drive.
//
// Note: tx_status_tests_cpp_parity_test.go in internal/chain/ covers
// a different upstream file — the chain-level tx_status enum tests
// (enum semantics, not the plugin's lookup index).

// transaction_status_test
func TestSkippedTransactionStatusTest(t *testing.T) {
	t.Skip("transaction_status_plugin (configurable nominal-depth tx-status lookup post-genesis) not ported; note: tx_status_tests_cpp_parity_test.go in internal/chain/ covers a different upstream file — the chain-level tx_status enum tests")
}
