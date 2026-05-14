package testfixture

import "testing"

// Skipped ports from hive/tests/unit/plugin_tests/pacemaker_tests.cpp.
//
// The C++ suite has one case (pacemaker_basic_test) that exercises
// the pacemaker_plugin: a wall-clock-paced block_log replay that
// throttles block application to mimic real-time block production.
// The Go port has no pacemaker_plugin — replay in Go applies blocks
// as fast as possible — so the test has nothing to drive.

// pacemaker_basic_test
func TestSkippedPacemakerBasicTest(t *testing.T) {
	t.Skip("pacemaker_plugin (wall-clock-paced block_log replay) not ported")
}
