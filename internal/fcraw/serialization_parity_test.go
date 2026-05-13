// Tests ported from hive/tests/unit/tests/serialization_tests.cpp covering
// fc::variant / fc::json behavior. Go has no fc::variant — JSON in this
// codebase goes through encoding/json (struct-typed) or static helpers in
// plugins/api. The C++ json_tests asserts that fc::json::variants_from_string
// accepts trailing whitespace in a numeric input; there's no equivalent path
// in fcraw (which is binary-only).
package fcraw

import "testing"

// TestJSONVariantsFromString ports
// serialization_tests.cpp::json_tests. The C++ side calls
// fc::json::variants_from_string("10.6 ") and "10.5" to verify
// trailing-whitespace tolerance. Go's encoding/json is strict and
// fundamentally typed; no analog exists in fcraw.
func TestJSONVariantsFromString(t *testing.T) {
	t.Skip("MISSING: fc::variant / fc::json::variants_from_string (Go uses encoding/json, not variants)")
}
