// Tests ported from hive/tests/unit/tests/serialization_tests.cpp.
package hardfork

import "testing"

// TestVersionStringForm ports the spirit of serialization_tests.cpp::
// version_test. The C++ side parses strings like "1.2.3" via fc::variant
// into hive::protocol::version. Go has only `PackedVersion(HFid)` returning
// the packed uint32 — there's no Version struct that round-trips through
// strings yet.
//
// TODO(version-port): add a `protocol/version` package exposing
//
//	type Version uint32
//	func ParseVersion(s string) (Version, error)
//	func (v Version) String() string
//
// matching the C++ rules:
//   - format major.minor.rev with '.' or '_' separators
//   - major, minor ≤ 255; rev ≤ 65535
//   - three-component parse only, no four-component "1.0.0.1"
//
// Until that's available the C++ version_test cannot be ported. We do
// confirm the bit-layout side (PackedVersion ↔ HF id) in version_test.go,
// which is the part our chain consumers actually use.
func TestVersionStringForm(t *testing.T) {
	t.Skip("TODO(version-port): need protocol/version with ParseVersion + String; see comment")
}

// TestHardforkVersionStringForm ports serialization_tests.cpp::
// hardfork_version_test. Same blocker as TestVersionStringForm: hived's
// hardfork_version is a version with rev=0, parsed/formatted as the same
// "M.m.r" syntax. PackedVersion in this package only converts an HF ID to
// the packed uint32; it doesn't accept strings.
//
// TODO(hardfork-version-port): once a `protocol/version` package lands,
// expose a HardforkVersion wrapper (Version with rev forced to 0) and port
// the rest of the C++ test cases (parse errors for 256.x.x, 1.0, 1.0.0.1).
func TestHardforkVersionStringForm(t *testing.T) {
	t.Skip("TODO(hardfork-version-port): need protocol/version with ParseHardforkVersion; see comment")
}
