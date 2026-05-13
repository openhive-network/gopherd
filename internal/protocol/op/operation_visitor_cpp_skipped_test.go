package op

import "testing"

// Skipped ports from hive/tests/unit/tests/operation_vistitor.cpp
// (the upstream filename includes the typo "vistitor", preserved here
// in the test-name suffix for grep-traceability).
//
// The upstream file exercises fc::static_variant's visitor pattern:
//
//	struct stub_visitor { template<typename T> void operator()(const T&); };
//	op.visit(stub_visitor{});
//
// Go has no equivalent template-based double-dispatch. The Go analogue
// is the explicit `switch o.Tag { case TagVote: ... }` pattern (see
// internal/protocol/op/op.go's per-tag readers table, or any consumer
// that switches on op.Op.Tag — e.g., evaluator dispatch in
// internal/chain). There is no language-level visitor to test.
//
// Both upstream cases are stubbed below with notes pointing to the Go
// equivalent.

// TestSkippedVisitPerformance — upstream visit_performance.
//
// The upstream test constructs each operation type by tag ID and calls
// op.visit(stub_visitor) 100_000_000 times per tag, logging ns/op as a
// micro-benchmark of fc::static_variant dispatch. Go's equivalent is a
// switch on op.Op.Tag (see opAsIface[] in tagged_op.go and readers[]
// in op.go) — neither of which is the same dispatch mechanism, so a
// 1:1 ns/op comparison would not be meaningful. Existing Go benchmarks
// for op dispatch live in internal/protocol/op/op_test.go and
// internal/protocol/op/tagged_op_test.go.
func TestSkippedVisitPerformance(t *testing.T) {
	t.Skip("fc::static_variant.visit() has no Go analogue; Op.Tag switch + readers[] table benchmark in op_test.go / tagged_op_test.go")
}

// TestSkippedBadObject — upstream bad_object.
//
// The upstream test constructs an operation holding an
// account_create_operation, asserts get<account_create_operation>()
// succeeds, then asserts get<transfer_operation>() throws
// fc::assert_exception. This exercises fc::static_variant's runtime
// type-check on the wrong-type accessor.
//
// Go's analogue is the typed AsX() accessor on Op (see ops_*.go),
// which is statically dispatched off the Tag and returns the zero
// value when the tag doesn't match. Type-confusion is a compile-time
// error in Go when the caller routes by Tag — there's no runtime
// assertion to trip. The "wrong-tag accessor returns the wrong
// thing" failure mode does not exist.
func TestSkippedBadObject(t *testing.T) {
	t.Skip("fc::static_variant runtime type-mismatch assertion has no Go analogue; Op.Tag switch is statically dispatched")
}
