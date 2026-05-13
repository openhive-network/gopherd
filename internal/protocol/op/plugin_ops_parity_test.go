package op

import "testing"

// Parity scaffolding for hive/tests/unit/plugin_tests/plugin_ops.cpp.
//
// Upstream layout note: the file plugin_ops.cpp itself is a stub --
// its entire body (a `test_plugin` with `test_a_operation` /
// `test_b_operation` and a generic_custom_operation_interpreter
// registration) is wrapped in a `/* ... */` block. There are no live
// BOOST_AUTO_TEST_CASEs in plugin_ops.cpp.
//
// The six named cases referenced in the porting brief
//
//   basic_validation, database_api_negative, misc_validation,
//   positive_validation, semantics_validation, syntax_validation
//
// actually live in hive/tests/unit/plugin_tests/json_rpc.cpp (suite
// `json_rpc`, fixture `json_rpc_database_fixture`). They are not
// operation-level validate() tests -- they exercise the JSON-RPC
// plugin's request framing: jsonrpc field, method field, params
// field, id field, raw JSON syntax errors, method routing, and
// `database_api.{find,list}_accounts` argument asserts.
//
// The Go equivalent of that surface lives in internal/api/jsonrpc
// (and per-API packages under internal/api/*), NOT in
// internal/protocol/op. Per the porting brief these stubs live in
// internal/protocol/op/ for grep-traceability with the rest of the
// op test corpus, but each test is skipped with a pointer to the
// correct subsystem -- there is nothing in package op to validate
// against for any of these cases.
//
// Each TestPluginOps* below preserves the upstream case name as a
// suffix so a developer chasing a regression in json_rpc.cpp lands
// on the matching Go stub via `go test -run TestPluginOps...`.

// TestPluginOpsBasicValidation -- upstream `basic_validation` in
// json_rpc.cpp. Sweeps malformed jsonrpc/method/params/id fields
// against `make_request(...)` and expects JSON_RPC_INVALID_REQUEST,
// JSON_RPC_PARSE_ERROR, JSON_RPC_PARSE_PARAMS_ERROR, JSON_RPC_NO_PARAMS,
// or JSON_RPC_SERVER_ERROR for each. Belongs in internal/api/jsonrpc.
func TestPluginOpsBasicValidation(t *testing.T) {
	t.Skip("json_rpc.cpp:basic_validation tests JSON-RPC request framing, not op.Validate(); belongs in internal/api/jsonrpc")
}

// TestPluginOpsSyntaxValidation -- upstream `syntax_validation` in
// json_rpc.cpp. Feeds raw-JSON syntax errors (unbalanced braces,
// truncated strings, missing values, bare identifiers, mid-batch
// failures) and expects JSON_RPC_SERVER_ERROR for each. Belongs in
// internal/api/jsonrpc.
func TestPluginOpsSyntaxValidation(t *testing.T) {
	t.Skip("json_rpc.cpp:syntax_validation tests JSON parser error reporting in the JSON-RPC plugin, not op.Validate(); belongs in internal/api/jsonrpc")
}

// TestPluginOpsMiscValidation -- upstream `misc_validation` in
// json_rpc.cpp. Exercises method-routing edge cases such as `a.b.c`,
// `a..c`, `fake_api.fake_method`, and `call` with a fake api/method
// pair; expects JSON_RPC_PARSE_PARAMS_ERROR for each. Belongs in
// internal/api/jsonrpc.
func TestPluginOpsMiscValidation(t *testing.T) {
	t.Skip("json_rpc.cpp:misc_validation tests JSON-RPC method routing, not op.Validate(); belongs in internal/api/jsonrpc")
}

// TestPluginOpsPositiveValidation -- upstream `positive_validation`
// in json_rpc.cpp. Asserts that well-formed `call` / dotted
// `<api>.<method>` requests against database_api, condenser_api, and
// block_api succeed via `make_positive_request`. Belongs in
// internal/api/jsonrpc plus the per-API packages
// (internal/api/database_api, condenser_api, block_api).
func TestPluginOpsPositiveValidation(t *testing.T) {
	t.Skip("json_rpc.cpp:positive_validation tests successful JSON-RPC dispatch into database_api/condenser_api/block_api, not op.Validate(); belongs in internal/api/jsonrpc and per-API packages")
}

// TestPluginOpsDatabaseApiNegative -- upstream `database_api_negative`
// in json_rpc.cpp. Asserts that `database_api.find_accounts` with an
// empty/missing args list and `database_api.list_accounts` with
// missing/zero limit or missing sort order yield
// JSON_RPC_ERROR_DURING_CALL with the specified assert messages.
//
// Skipped per user direction: API tests are not in scope for the
// protocol/op port.
func TestPluginOpsDatabaseApiNegative(t *testing.T) {
	t.Skip("N/A: API tests skipped per user direction")
}

// TestPluginOpsSemanticsValidation -- upstream `semantics_validation`
// in json_rpc.cpp. Verifies that JSON-RPC `id` may be a positive int,
// a negative int, or a numeric string of either, by sending four
// requests through `make_positive_request`. Belongs in
// internal/api/jsonrpc.
func TestPluginOpsSemanticsValidation(t *testing.T) {
	t.Skip("json_rpc.cpp:semantics_validation tests JSON-RPC id-field type acceptance, not op.Validate(); belongs in internal/api/jsonrpc")
}
