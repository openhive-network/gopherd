package testfixture

import "testing"

// Skipped ports from hive/tests/unit/plugin_tests/witness_tests.cpp.
//
// These cases exercise witness-plugin behaviors that depend on Go-side
// subsystems which are not yet implemented. They are tracked here as
// `t.Skip` inventory stubs so that a grep for the upstream
// BOOST_AUTO_TEST_CASE name lands directly on the placeholder and the
// rationale records which subsystem is blocking the port.
//
// Missing Go-side features and their case mapping:
//
//   - Block-supplementation path on the witness/p2p boundary:
//       supplemented_blocks_test
//   - Producer-level fork-conflict resolution (Go fork-DB lives in
//     the chain layer; the producer thread has no equivalent hook):
//       block_conflict_test
//   - Production-thread block-lock (one-block-per-witness-per-slot):
//       block_lock_test
//   - Production-loop lag / skip-slot logic:
//       block_lag_test
//   - OBI (One-Block-Irreversibility) plugin:
//       slow_obi_test
//   - Colony synthetic-load plugin:
//       colony_basic_test, colony_no_workers_test
//   - Queen mode (single-node block production) in the witness plugin:
//       basic_queen_test
//   - Colony + Queen combined operation:
//       colony_queen_test
//
// If any of these plugins/features land, replace the matching Skip
// with an actual port — the C++ scenarios translate fairly directly
// once the underlying subsystem exists.

// supplemented_blocks_test — peer-supplemented block fill during sync.
func TestSkippedSupplementedBlocks(t *testing.T) {
	t.Skip("witness plugin has no block-supplementation path; gossip layer handles missing blocks differently")
}

// block_conflict_test — fork resolution at producer level.
func TestSkippedBlockConflict(t *testing.T) {
	t.Skip("producer-level fork conflict handling not implemented; fork DB is handled in chain layer")
}

// block_lock_test — one-block-per-witness-per-slot production lock.
func TestSkippedBlockLock(t *testing.T) {
	t.Skip("production-thread block-lock not implemented")
}

// block_lag_test — production-thread lag behavior.
func TestSkippedBlockLag(t *testing.T) {
	t.Skip("production-loop lag/skip-slot logic not implemented")
}

// slow_obi_test — OBI under slow network.
func TestSkippedSlowObi(t *testing.T) {
	t.Skip("OBI (One-Block-Irreversibility) plugin not implemented")
}

// colony_basic_test — colony synthetic-load plugin.
func TestSkippedColonyBasic(t *testing.T) {
	t.Skip("Colony plugin not implemented")
}

// colony_no_workers_test — colony degenerate config.
func TestSkippedColonyNoWorkers(t *testing.T) {
	t.Skip("Colony plugin not implemented")
}

// basic_queen_test — single-node queen mode.
func TestSkippedBasicQueen(t *testing.T) {
	t.Skip("Queen mode not implemented in witness plugin")
}

// colony_queen_test — colony + queen combination.
func TestSkippedColonyQueen(t *testing.T) {
	t.Skip("Colony + Queen modes not implemented")
}
