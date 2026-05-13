// Tests ported from hive/tests/unit/tests/serialization_tests.cpp covering
// operation-level JSON / static_variant behavior. Go uses a typed Op
// interface with a Tag enum instead of fc::static_variant — there's no
// shared codec for the [tag-name, payload] array form or the
// {"type":"...","value":{...}} object form. Both are condenser_api concerns,
// not consensus, and we defer until those plugins are ported.
package op

import "testing"

// TestStaticVariantJSONForm ports
// serialization_tests.cpp::static_variant_json_test. The C++ side uses
// fc::static_variant<transfer_operation, comment_operation>'s JSON form
// ({"type":"transfer_operation","value":{...}}) and asserts which-tag-by-
// name parsing. Go's Op is a tagged union (Tag enum, typed accessors) — the
// JSON wrappers belong to plugins/api, not op/.
func TestStaticVariantJSONForm(t *testing.T) {
	t.Skip("MISSING: static_variant JSON codec (type/value object form lives in plugins/api)")
}
