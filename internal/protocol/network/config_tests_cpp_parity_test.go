package network

import "testing"

// Mirrors hive/tests/unit/tests/config_tests.cpp.
//
// The upstream file is exclusively a logging-config test (it boots a
// hived_fixture, calls postponed_init() with various log-appender /
// log-logger config.ini lines, and asserts the JSON-serialized
// fc::logging_config matches a hand-written pattern). The Go codebase
// has no equivalent of fc::logging_config — log output is driven by
// the standard library logger + per-package wiring — so the three
// upstream test cases have no portable analogue and are stubbed at
// the bottom of this file.
//
// What IS portable is the parity check on HIVE_* consensus constants
// that the upstream fixture relies on at boot. Those identities are
// frozen by the C++ config.hpp and our network package must hold them
// bit-identical. The tests below verify the arithmetic identities and
// pin the integer values to their upstream literals.

// TestHiveBlockIntervalIs3 — config.hpp:
//
//	#define HIVE_BLOCK_INTERVAL 3
func TestHiveBlockIntervalIs3(t *testing.T) {
	if BlockIntervalSec != 3 {
		t.Fatalf("BlockIntervalSec = %d, want 3 (HIVE_BLOCK_INTERVAL)", BlockIntervalSec)
	}
}

// TestHiveBlocksPerHourIdentity — config.hpp:
//
//	#define HIVE_BLOCKS_PER_HOUR (60*60/HIVE_BLOCK_INTERVAL)
func TestHiveBlocksPerHourIdentity(t *testing.T) {
	if BlocksPerHour != 60*60/BlockIntervalSec {
		t.Fatalf("BlocksPerHour = %d, want %d", BlocksPerHour, 60*60/BlockIntervalSec)
	}
	if BlocksPerHour != 1200 {
		t.Fatalf("BlocksPerHour = %d, want 1200 (HIVE_BLOCKS_PER_HOUR)", BlocksPerHour)
	}
}

// TestHiveBlocksPerDayIdentity — config.hpp:
//
//	#define HIVE_BLOCKS_PER_DAY (24*60*60/HIVE_BLOCK_INTERVAL)
func TestHiveBlocksPerDayIdentity(t *testing.T) {
	if BlocksPerDay != 24*60*60/BlockIntervalSec {
		t.Fatalf("BlocksPerDay = %d, want %d", BlocksPerDay, 24*60*60/BlockIntervalSec)
	}
	if BlocksPerDay != 24*BlocksPerHour {
		t.Fatalf("BlocksPerDay = %d, want 24*BlocksPerHour = %d", BlocksPerDay, 24*BlocksPerHour)
	}
	if BlocksPerDay != 28800 {
		t.Fatalf("BlocksPerDay = %d, want 28800 (HIVE_BLOCKS_PER_DAY)", BlocksPerDay)
	}
}

// TestHiveBlocksPerYearIdentity — config.hpp:
//
//	#define HIVE_BLOCKS_PER_YEAR (365*24*60*60/HIVE_BLOCK_INTERVAL)
func TestHiveBlocksPerYearIdentity(t *testing.T) {
	if BlocksPerYear != 365*24*60*60/BlockIntervalSec {
		t.Fatalf("BlocksPerYear = %d, want %d", BlocksPerYear, 365*24*60*60/BlockIntervalSec)
	}
	if BlocksPerYear != 365*BlocksPerDay {
		t.Fatalf("BlocksPerYear = %d, want 365*BlocksPerDay = %d", BlocksPerYear, 365*BlocksPerDay)
	}
	if BlocksPerYear != 10512000 {
		t.Fatalf("BlocksPerYear = %d, want 10512000 (HIVE_BLOCKS_PER_YEAR)", BlocksPerYear)
	}
}

// TestHiveMaxWitnessesIs21 — config.hpp:
//
//	#define HIVE_MAX_WITNESSES 21
//
// The witness-slot split into voted/miner/runner sums to this value
// and is verified in internal/state/objects/witness_schedule_cpp_parity_test.go
// (kept there to avoid a network→objects import edge).
func TestHiveMaxWitnessesIs21(t *testing.T) {
	if MaxWitnesses != 21 {
		t.Fatalf("MaxWitnesses = %d, want 21 (HIVE_MAX_WITNESSES)", MaxWitnesses)
	}
}

// --- Skip stubs for the three upstream BOOST_AUTO_TEST_CASEs that
// drive fc::logging_config — Go has no analogue. ---

// TestSkippedDefaultLoggingConfig — upstream default_logging_config_test.
// Asserts that the auto-generated config.ini produces a specific
// fc::logging_config JSON (stderr + p2p file appender, three loggers).
// Go logging is the standard library log + per-package wiring; there is
// no config.ini-driven appender/logger graph to compare against.
func TestSkippedDefaultLoggingConfig(t *testing.T) {
	t.Skip("fc::logging_config has no Go analogue; Go logging is stdlib log + per-package wiring")
}

// TestSkippedCustomLoggingConfig — upstream custom_logging_config_test.
// Same rationale as above: drives log-appender config.ini lines and
// asserts JSON serialization of the resulting fc::appender_config.
func TestSkippedCustomLoggingConfig(t *testing.T) {
	t.Skip("fc::appender_config has no Go analogue; no config.ini logging surface in Go")
}

// TestSkippedLoggingRotation — upstream rotation_test. Drives a 1-second
// rotation_interval + 5-second rotation_limit, sleeps 5 seconds writing
// ilog() messages, and counts files in logs/p2p/. Time-based filesystem
// rotation is not implemented in Go's logging (no equivalent of fc's
// file_appender::rotate_files). When/if rotation lands, port from
// config_tests.cpp:96.
func TestSkippedLoggingRotation(t *testing.T) {
	t.Skip("file_appender rotation has no Go analogue; not implemented")
}
