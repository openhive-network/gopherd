package testfixture

import "testing"

// Skipped ports from hive/tests/unit/tests/operation_tests.cpp::timeshare_tests.
//
// These three cases exercise the C++ `timeshare_test_fixture<N>` template
// fixture, which spins up 10 timeshare witnesses plus dedicated voters and
// drives the virtual-schedule slot under HF25/HF26/HF27 semantics. They
// depend on infrastructure not yet ported to the Go testfixture: the
// `timeshare_test_fixture` template fixture itself and the
// `account_subsidy_*` `set_witness_props` evaluator extension that the
// fixture uses to configure per-witness subsidy budgets. The same gap is
// already noted on TestHF23AccountSubsidyWitnessLimits in hf23_extra_test.go.

// test_pre_hf26_behavior
func TestSkippedTestPreHf26Behavior(t *testing.T) {
	t.Skip("timeshare_test_fixture template fixture + account_subsidy_* set_witness_props plumbing not wired into Go testfixture")
}

// test_hf26_behavior
func TestSkippedTestHf26Behavior(t *testing.T) {
	t.Skip("timeshare_test_fixture template fixture + account_subsidy_* set_witness_props plumbing not wired into Go testfixture")
}

// test_hf27_behavior
func TestSkippedTestHf27Behavior(t *testing.T) {
	t.Skip("timeshare_test_fixture template fixture + account_subsidy_* set_witness_props plumbing not wired into Go testfixture")
}
