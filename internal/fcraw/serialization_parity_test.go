// Tests ported from hive/tests/unit/tests/serialization_tests.cpp covering
// fc::variant / fc::json behavior. Go has no fc::variant — JSON in this
// codebase goes through encoding/json (struct-typed) or static helpers in
// plugins/api. The C++ json_tests asserts that fc::json::variants_from_string
// accepts trailing whitespace in a numeric input; there's no equivalent path
// in fcraw (which is binary-only).
package fcraw

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestJSONVariantsFromString ports
// serialization_tests.cpp::json_tests. The C++ side calls
// fc::json::variants_from_string("10.6 ") and "10.5" to verify
// trailing-whitespace tolerance. Go's encoding/json is strict and
// fundamentally typed; no analog exists in fcraw.
func TestJSONVariantsFromString(t *testing.T) {
	t.Skip("MISSING: fc::variant / fc::json::variants_from_string (Go uses encoding/json, not variants)")
}

// TestIncorrectJSONDeserialization ports
// serialization_tests.cpp::incorrect_json_deserialization. The C++ test
// dumps a truncated JSON array literal into a file and asserts that both
// fc::json::from_string and fc::json::from_file throw fc::parse_error
// in format_validation_mode::full. Go's encoding/json reports the same
// truncation as io.ErrUnexpectedEOF / SyntaxError. We verify both the
// in-memory parse and the from-file path.
func TestIncorrectJSONDeserialization(t *testing.T) {
	content := "[{\n" +
		"    \"endpoint\": \"2.59.255.86:2001\",\n" +
		"    \"last_seen_time\": \"2024-11-13T10:00:10\",\n" +
		"    \"last_connection_disposition\": \"last_connection_handshaking_failed\",\n" +
		"    \"la\n"

	var v interface{}
	if err := json.Unmarshal([]byte(content), &v); err == nil {
		t.Fatal("expected json.Unmarshal to reject truncated input, got nil")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	if err := json.Unmarshal(data, &v); err == nil {
		t.Fatal("expected json.Unmarshal of file contents to reject truncated input, got nil")
	}
}
