package asset

import "testing"

// TestParseLegacyAsset ports serialization_tests.cpp::legacy_asset_test.
// Verifies legacy_asset::from_string accepts canonical forms and rejects
// the documented set of malformed inputs.
func TestParseLegacyAsset(t *testing.T) {
	good := []struct {
		in     string
		amount int64
		sym    AssetNum
	}{
		{"123.456 HIVE", 123456, HiveSymbol},
		{"123.456 TESTS", 123456, HiveSymbol},
		{"654.321 HBD", 654321, HbdSymbol},
		{"654.321 TBD", 654321, HbdSymbol},
		{"0.456 HIVE", 456, HiveSymbol},
		{"0.056 HIVE", 56, HiveSymbol},
		{"0.050 HIVE", 50, HiveSymbol},
		{"50.000 HBD", 50000, HbdSymbol},
		{"-1.234 HIVE", -1234, HiveSymbol},
	}
	for _, tc := range good {
		a, err := ParseLegacyAsset(tc.in)
		if err != nil {
			t.Errorf("ParseLegacyAsset(%q): unexpected error %v", tc.in, err)
			continue
		}
		if a.Amount != tc.amount {
			t.Errorf("ParseLegacyAsset(%q): amount=%d, want %d", tc.in, a.Amount, tc.amount)
		}
		if a.Symbol.Num != tc.sym {
			t.Errorf("ParseLegacyAsset(%q): sym=%d, want %d", tc.in, a.Symbol.Num, tc.sym)
		}
	}

	// Mirror the C++ throw-list at lines 169-187.
	bad := []string{
		"1.00000000000000000000 TESTS", // too many decimals
		"1.000TESTS",                   // missing space
		"1. 333 TESTS",                 // space inside amount
		"1 .333 TESTS",                 // space inside amount
		"1. 333 X",                     // unknown symbol
		"1 .333 X",                     // unknown symbol
		"1 .333",                       // no symbol after split
		"1 1.1",                        // multiple spaces, three tokens
		"11111111111111111111111111111111111111111111111 TESTS", // overflow
		"1.1.1 TESTS", // multiple dots
		"1.abc TESTS", // non-digit in frac
		" TESTS",      // missing amount
		"TESTS",       // single token
		"1.333",       // no symbol
		"1.333 ",      // trailing space
		"",            // empty
		" ",           // whitespace only
		"  ",          // whitespace only
		"100 TESTS",   // missing decimals
	}
	for _, in := range bad {
		_, err := ParseLegacyAsset(in)
		if err == nil {
			t.Errorf("ParseLegacyAsset(%q): expected error, got success", in)
		}
	}
}

// TestAssetStringRoundTrip ports the round-trip portion of asset_test.
func TestAssetStringRoundTrip(t *testing.T) {
	cases := []struct {
		amount int64
		sym    AssetNum
		want   string
	}{
		// Rendering is per-network (HIVE/HBD on mainnet, TESTS/TBD on a testnet
		// build); PARSING accepts both spellings on either, which is why the
		// round-trip below holds regardless.
		{123456, HiveSymbol, "123.456 " + displayHiveName},
		{50, HiveSymbol, "0.050 " + displayHiveName},
		{50000, HiveSymbol, "50.000 " + displayHiveName},
		{50, HbdSymbol, "0.050 " + displayHbdName},
		{654321, HbdSymbol, "654.321 " + displayHbdName},
		{0, HiveSymbol, "0.000 " + displayHiveName},
	}
	for _, c := range cases {
		a := Asset{Amount: c.amount, Symbol: Symbol{Num: c.sym}}
		got := a.String()
		if got != c.want {
			t.Errorf("Asset.String: amount=%d sym=%d → %q, want %q",
				c.amount, c.sym, got, c.want)
		}
		// Round-trip via ParseLegacyAsset.
		parsed, err := ParseLegacyAsset(got)
		if err != nil {
			t.Errorf("round-trip parse %q: %v", got, err)
			continue
		}
		if parsed.Amount != c.amount {
			t.Errorf("round-trip %q: amount=%d, want %d", got, parsed.Amount, c.amount)
		}
	}
}
