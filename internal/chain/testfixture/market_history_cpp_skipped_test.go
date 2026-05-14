package testfixture

import "testing"

// Skipped ports from hive/tests/unit/plugin_tests/market_history.cpp.
//
// The C++ suite has one case (mh_test) that drives the
// market_history_plugin: it places limit orders, fills them, and
// asserts the plugin's bucket aggregation (per-interval OHLCV) and
// order_history index. The Go port has no market_history_plugin —
// the consensus-level limit-order behaviour itself (matching,
// partial fills, expirations) is covered by TestParity_* in
// op_market_test.go.

// mh_test
func TestSkippedMhTest(t *testing.T) {
	t.Skip("market_history_plugin bucket/order_history aggregation not ported; consensus-level limit-order behaviour is covered by TestParity_* in op_market_test.go")
}
