package snapshot

import "testing"

// Skipped ports from hive/tests/unit/plugin_tests/snapshots_tests.cpp.
//
// The C++ suite has one case
// (additional_allocation_after_snapshot_load) that asserts chainbase's
// get_item_additional_allocation() — an allocator-stat introspection
// API that reports per-object overhead allocated outside the
// fixed-size multi_index node (e.g. shared-memory-backed strings).
// The Go runtime owns the heap, so there is no equivalent
// introspection point and the assertion has nothing to compare
// against. The actual snapshot save/load round-trip is covered by
// TestSaveLoadRoundtrip in snapshot_test.go.

// additional_allocation_after_snapshot_load
func TestSkippedAdditionalAllocationAfterSnapshotLoad(t *testing.T) {
	t.Skip("chainbase get_item_additional_allocation() allocator-stat introspection has no Go analogue (Go runtime owns heap); snapshot round-trip parity itself is covered by TestSaveLoadRoundtrip")
}
