package auth

import "testing"

// TestIsValidAccountName ports basic_tests.cpp::valid_name_test in
// libraries/chain/tests. Mirrors every individual BOOST_CHECK so any
// future regression in the validator surfaces with the same C++
// rejection reason. Verifies the mainnet name ruleset:
//   - dot-separated segments of [3, n] chars
//   - segment starts with lowercase letter
//   - segment ends with [a-z0-9]
//   - inner chars [a-z0-9-]
//   - total length in [3, 16]
//
// Note: the upstream max-account-name length cap is 16 in mainnet
// config.hpp, but the C++ test allows 63-char labels to test the
// segment shape. Our test uses the max-length cases that satisfy
// HIVE_MAX_ACCOUNT_NAME_LENGTH = 16.
func TestIsValidAccountName(t *testing.T) {
	good := []string{
		"aaa", "a0a", "a-a", "aa0", "a00", "a-0",
		"alice", "bob", "abc",
		"alice12", "alice-bob",
		"abc.def.ghi",
		"alpha9",
		"a1b2c3d4e5f6",
		"aaa-bbb-ccc",
		"aaa-bbb.ccc",
		"aaa.bbb-ccc",
		"aaa.bbb.ccc",
		"aaa--bbb--cc",
		"this-label-has",
	}
	for _, n := range good {
		if !IsValidAccountName(n) {
			t.Errorf("expected valid: %q", n)
		}
	}

	bad := []string{
		// single-char rejects
		"a", "A", "0", ".", "-",
		// two-char rejects
		"aa", "aA", "a0", "a.", "a-",
		// uppercase mid-segment
		"aAa", "aA0",
		// dot or invalid char mid-segment
		"a.a", "a.0",
		// invalid char in segment
		"aaa,bbb-ccc", "aaa_bbb-ccc", "aaa-BBB-ccc",
		// starts with digit/dash/dot/slash
		"1aaa-bbb", "-aaa-bbb-ccc", ".aaa-bbb-ccc", "/aaa-bbb-ccc",
		// trailing dash/dot/slash/double-dot
		"aaa-bbb-ccc-", "aaa-bbb-ccc.", "aaa-bbb-ccc..", "aaa-bbb-ccc/",
		// double-dot
		"aaa..bbb-ccc",
		// segment too short
		"xn--san-p8a.de", "xn-san-p8a.de",
		// generic structural
		"", "al",
		"abcdefghijklmnopq", // 17 chars > MaxAccountNameLength=16
		"alice-", "-alice", "123abc",
		"alice..bob", "alice.b", ".alice", "alice.",
		"a.b.c", "a.bcd", "Alice", "alice_bob", "alice/bob",
	}
	for _, n := range bad {
		if IsValidAccountName(n) {
			t.Errorf("expected invalid: %q", n)
		}
	}
}
