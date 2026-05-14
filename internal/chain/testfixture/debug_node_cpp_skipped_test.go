package testfixture

import "testing"

// Skipped ports from hive/tests/unit/plugin_tests/debug_node.cpp.
//
// All 5 cases drive state through debug_node_plugin's
// db_plugin->debug_update / calculate_modifiers_according_to_new_price
// hook chain. The debug_node_plugin is a hived-internal testing
// affordance that lets a test inject arbitrary mutations between
// blocks; the Go port has no equivalent (see undo_cpp_parity_test.go
// and fixture.go for the existing note explaining this gap), so each
// case is tracked here as a stub.
//
//   - vests_hive_evaluation              VESTS↔HIVE price-feed driven
//                                        modifiers under debug-update.
//   - state_modification                 raw debug-update mutation of a
//                                        DGPO field, then assert.
//   - debug_update_use_bug               historical bug where the same
//                                        debug-update fired twice; pins
//                                        the regression fix.
//   - debug_update_with_explicit_hook    user supplies the hook block#.
//   - debug_update_transaction_order     debug-updates ordered against
//                                        normal transactions in a block.

// vests_hive_evaluation
func TestSkippedVestsHiveEvaluation(t *testing.T) {
	t.Skip("depends on debug_node_plugin db_plugin->debug_update / calculate_modifiers_according_to_new_price hook chain; debug_node_plugin not ported (see undo_cpp_parity_test.go and fixture.go for the existing note)")
}

// state_modification
func TestSkippedStateModification(t *testing.T) {
	t.Skip("depends on debug_node_plugin db_plugin->debug_update / calculate_modifiers_according_to_new_price hook chain; debug_node_plugin not ported (see undo_cpp_parity_test.go and fixture.go for the existing note)")
}

// debug_update_use_bug
func TestSkippedDebugUpdateUseBug(t *testing.T) {
	t.Skip("depends on debug_node_plugin db_plugin->debug_update / calculate_modifiers_according_to_new_price hook chain; debug_node_plugin not ported (see undo_cpp_parity_test.go and fixture.go for the existing note)")
}

// debug_update_with_explicit_hook
func TestSkippedDebugUpdateWithExplicitHook(t *testing.T) {
	t.Skip("depends on debug_node_plugin db_plugin->debug_update / calculate_modifiers_according_to_new_price hook chain; debug_node_plugin not ported (see undo_cpp_parity_test.go and fixture.go for the existing note)")
}

// debug_update_transaction_order
func TestSkippedDebugUpdateTransactionOrder(t *testing.T) {
	t.Skip("depends on debug_node_plugin db_plugin->debug_update / calculate_modifiers_according_to_new_price hook chain; debug_node_plugin not ported (see undo_cpp_parity_test.go and fixture.go for the existing note)")
}
