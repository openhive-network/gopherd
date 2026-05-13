package state

import "testing"

// Skipped ports from hive/tests/unit/tests/bmic_tests.cpp.
//
// The C++ suite exercises boost::multi_index_container behaviour
// directly: ordered/composite ordered indices, insert/remove with
// collisions, in-place modify, and miscellaneous corner cases over
// three synthetic test_object types.
//
// hived-go does NOT use boost::multi_index. The equivalent indexing
// layer is built on tidwall/btree (see internal/state/mem.go +
// internal/state/objects/*_store.go). Per project memory
// (project_typed_store_overhaul_2026_05_07.md) the typed K/V backing
// store gets its coverage from:
//
//   - internal/state/state_test.go            (Get/Put/Delete/Commit/Restore)
//   - internal/state/encoding_test.go         (key/value codec)
//   - internal/state/objects/*_test.go        (per-typed-store insert/
//     remove/modify under Commit + Restore)
//
// The bmic suite's test_object_index / test_object_index2 /
// test_object_index3 types are pure C++ index-machinery exercises; no
// portable assertion in Go-land would touch boost-specific behaviour
// (e.g., iterator invalidation on modify_key collision).

// TestSkippedBmicBasicTests — basic_tests: insert + lookup across
// ordered_unique/ordered_non_unique/composite indices of test_object,
// test_object2, test_object3.
func TestSkippedBmicBasicTests(t *testing.T) {
	t.Skip("boost::multi_index internals; Go uses tidwall/btree per typed-store, covered by internal/state/state_test.go")
}

// TestSkippedBmicInsertRemoveTests — insert_remove_tests: emplace +
// erase + assert all secondary index views stay consistent.
func TestSkippedBmicInsertRemoveTests(t *testing.T) {
	t.Skip("boost::multi_index internals; Go's per-typed-store insert/remove covered elsewhere")
}

// TestSkippedBmicInsertRemoveCollisionTests — insert_remove_collision_tests:
// inserts where the (id) key collides; exercises ordered_unique
// rejection and composite key collisions.
func TestSkippedBmicInsertRemoveCollisionTests(t *testing.T) {
	t.Skip("boost::multi_index internals; Go btree primary-key collision behaviour differs by store")
}

// TestSkippedBmicModifyTests — modify_tests: in-place modify() with a
// mutator lambda, asserts secondary indexes track the change; tests
// modify-returning-false on collision.
func TestSkippedBmicModifyTests(t *testing.T) {
	t.Skip("boost::multi_index internals; Go typed-stores re-Put on mutate")
}

// TestSkippedBmicMiscTests — misc_tests: iterator round-trips across
// OrderedIndex / CompositeOrderedIndex on test_object.
func TestSkippedBmicMiscTests(t *testing.T) {
	t.Skip("boost::multi_index iterator semantics; not applicable to tidwall/btree")
}

// TestSkippedBmicMiscTests3 — misc_tests3: composite-index iterator
// tests over test_object3 (two composite views).
func TestSkippedBmicMiscTests3(t *testing.T) {
	t.Skip("boost::multi_index iterator semantics; not applicable to tidwall/btree")
}
