// Tests ported from hive/tests/unit/tests/serialization_tests.cpp covering
// asset / asset_symbol_type binary and bit-layout behavior. JSON-variant
// cases that rely on fc::variant for asset are skipped (Go uses
// encoding/json, not fc::json, and asset has no encoding/json codec by
// design — wire form is canonical).
package asset

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"

	"gopherd/internal/fcraw"
)

// hiveMaxShareSupply mirrors HIVE_MAX_SHARE_SUPPLY (1e15 * 1e3 sat = 1e18).
// Use the same value the C++ test compares against (testnet inherits the
// mainnet supply ceiling).
const hiveMaxShareSupply int64 = 1_000_000_000_000_000_000

// TestAssetRawRoundTripSweep ports
// serialization_tests.cpp::asset_raw_test. The C++ test takes 64 amounts
// (computed from low bits of sha256("")) crossed with HIVE/HBD/VESTS
// symbols, packs each via fc::raw, manually re-builds the 16-byte legacy
// frame, and asserts byte equality; it then unpacks and asserts round-trip.
//
// Go's legacy encoding is the 8-byte (precision || name) form with
// well-known names "STEEM\0\0" (HIVE), "SBD\0\0\0\0" (HBD), "VESTS\0\0"
// (VESTS). We reproduce the same 16-byte expected layout and compare.
func TestAssetRawRoundTripSweep(t *testing.T) {
	// The expected hex below was captured from MAINNET hived, so it embeds the
	// mainnet legacy symbol names (STEEM/SBD). A testnet build legitimately
	// emits TESTS/TBD, which this table cannot express; the encoder itself is
	// covered per-network by TestLegacySymbolDecode.
	if legacyHiveName != "STEEM" {
		t.Skip("mainnet C++ parity vectors: testnet uses different legacy symbol names")
	}
	// Confirm the ordering invariants the C++ test asserts up front.
	if !(HbdSymbol < HiveSymbol) {
		t.Errorf("expected HBD asset_num < HIVE asset_num, got %#x vs %#x",
			uint32(HbdSymbol), uint32(HiveSymbol))
	}
	if !(HiveSymbol < VestsSymbol) {
		t.Errorf("expected HIVE asset_num < VESTS asset_num, got %#x vs %#x",
			uint32(HiveSymbol), uint32(VestsSymbol))
	}

	// Build the same amount set: low N bits of sha256("")[0..8].
	digest := sha256.Sum256(nil)
	low8 := uint64(digest[0]) | uint64(digest[1])<<8 | uint64(digest[2])<<16 |
		uint64(digest[3])<<24 | uint64(digest[4])<<32 | uint64(digest[5])<<40 |
		uint64(digest[6])<<48 | uint64(digest[7])<<56

	var amounts []int64
	for i := 0; i < 64; i++ {
		s := uint64(1) << i
		x := (low8 & (s - 1)) | s
		if int64(x) >= hiveMaxShareSupply || int64(x) < 0 {
			break
		}
		amounts = append(amounts, int64(x))
	}
	if len(amounts) < 30 {
		t.Fatalf("amount sweep too short (%d) — sha256(\"\") low8 = %#x", len(amounts), low8)
	}

	type symCase struct {
		sym     Symbol
		oldPack []byte
	}
	cases := []symCase{
		{
			Symbol{Num: HiveSymbol, WasLegacyEncoded: true},
			[]byte{0x03, 'S', 'T', 'E', 'E', 'M', 0x00, 0x00},
		},
		{
			Symbol{Num: HbdSymbol, WasLegacyEncoded: true},
			[]byte{0x03, 'S', 'B', 'D', 0x00, 0x00, 0x00, 0x00},
		},
		{
			Symbol{Num: VestsSymbol, WasLegacyEncoded: true},
			[]byte{0x06, 'V', 'E', 'S', 'T', 'S', 0x00, 0x00},
		},
	}

	for _, amt := range amounts {
		for _, c := range cases {
			a := Asset{Amount: amt, Symbol: c.sym}

			// Expected layout: 8 bytes LE amount || 8-byte legacy symbol.
			want := make([]byte, 0, 16)
			x := uint64(amt)
			for i := 0; i < 8; i++ {
				want = append(want, byte(x))
				x >>= 8
			}
			want = append(want, c.oldPack...)

			got := AppendAsset(nil, a)
			if !bytes.Equal(got, want) {
				t.Errorf("AppendAsset(%d, %s) = %x, want %x",
					amt, c.sym.String(), got, want)
				continue
			}

			// Unpack symmetry: decoded asset compares equal.
			r := fcraw.NewReader(got)
			a2, err := ReadAsset(r)
			if err != nil {
				t.Errorf("ReadAsset(%x): %v", got, err)
				continue
			}
			if a2.Amount != a.Amount || a2.Symbol.Num != a.Symbol.Num {
				t.Errorf("round-trip mismatch: got %+v, want %+v", a2, a)
			}
			if r.Remaining() != 0 {
				t.Errorf("trailing bytes after ReadAsset: %d", r.Remaining())
			}
		}
	}
}

// TestAssetSymbolTypeNAIRoundTrip ports
// serialization_tests.cpp::asset_symbol_type_test. The C++ test exercises
// asset_symbol_type::from_asset_num / from_nai / from_nai_string and the
// paired-symbol helper. Go's asset.Symbol carries the same packed
// (NAI << 5) | precision in Num; this test confirms the bit layout.
//
// We deliberately skip the from_nai_string portion (Go has no NAI-string
// parser; symbols are read either from the 4-byte NAI form on the wire or
// the legacy 8-byte form).
func TestAssetSymbolTypeNAIRoundTrip(t *testing.T) {
	// HIVE_NAI_SHIFT = 5, precision in the low 4 bits, control bit at 4.
	const naiShift = 5
	const smtControlMask uint32 = 1 << 4

	// Build an asset_num from a 7-digit NAI base value, precision=3, with
	// the SMT control bit set. Matches the C++ test's asset_num assembly:
	//   asset_num = (10000000 << 5) | SMT_ASSET_NUM_CONTROL_MASK | 3
	assetNum := (uint32(10000000) << naiShift) | smtControlMask | 3

	s := Symbol{Num: AssetNum(assetNum)}
	if got := s.Decimals(); got != 3 {
		t.Errorf("Decimals() = %d, want 3", got)
	}
	// Precision survives in the low 4 bits.
	if uint32(s.Num)&0xF != 3 {
		t.Errorf("precision bits = %d, want 3", uint32(s.Num)&0xF)
	}
	// SMT control bit survives at bit 4.
	if uint32(s.Num)&smtControlMask == 0 {
		t.Errorf("control bit missing in asset_num %#x", uint32(s.Num))
	}

	// Round-trip via the 4-byte NAI wire form.
	wire := MarshalNAI(nil, s)
	if len(wire) != 4 {
		t.Fatalf("MarshalNAI: %d bytes, want 4", len(wire))
	}
	got, err := ReadSymbol(fcraw.NewReader(wire))
	if err != nil {
		t.Fatalf("ReadSymbol(NAI): %v", err)
	}
	if got.Num != s.Num {
		t.Errorf("NAI round-trip: got %#x, want %#x", uint32(got.Num), uint32(s.Num))
	}
	if got.WasLegacyEncoded {
		t.Errorf("NAI round-trip should not flag WasLegacyEncoded")
	}

	// Well-known legacy symbols decode into their canonical asset_num.
	hiveSym := Symbol{Num: HiveSymbol, WasLegacyEncoded: true}
	hbdSym := Symbol{Num: HbdSymbol, WasLegacyEncoded: true}
	vestsSym := Symbol{Num: VestsSymbol, WasLegacyEncoded: true}
	for _, sym := range []Symbol{hiveSym, hbdSym, vestsSym} {
		legacy, err := MarshalLegacy(nil, sym)
		if err != nil {
			t.Fatalf("MarshalLegacy(%s): %v", sym.String(), err)
		}
		got, err := ReadSymbol(fcraw.NewReader(legacy))
		if err != nil {
			t.Fatalf("ReadSymbol(legacy %s): %v", sym.String(), err)
		}
		if got.Num != sym.Num {
			t.Errorf("legacy round-trip %s: got %#x, want %#x",
				sym.String(), uint32(got.Num), uint32(sym.Num))
		}
		if !got.WasLegacyEncoded {
			t.Errorf("legacy decode of %s did not set WasLegacyEncoded", sym.String())
		}
	}
}

// TestAssetJSONForm ports the JSON portion of
// serialization_tests.cpp::asset_test. The C++ side serializes an asset via
// fc::json and round-trips through `{"amount":"123456","precision":3,
// "nai":"@@000000021"}`.
func TestAssetJSONForm(t *testing.T) {
	// Empty asset (zero amount, HIVE) serializes as the canonical empty form.
	// Mirrors `fc::json::to_string( asset() )`.
	empty := Asset{Amount: 0, Symbol: Symbol{Num: HiveSymbol}}
	if got, _ := json.Marshal(empty); string(got) != `{"amount":"0","precision":3,"nai":"@@000000021"}` {
		t.Errorf("empty asset JSON = %s, want %s",
			got, `{"amount":"0","precision":3,"nai":"@@000000021"}`)
	}

	// Round-trip from the three well-known NAI strings.
	type rtCase struct {
		in        string
		wantAmt   int64
		wantDec   uint8
		wantSym   AssetNum
		canonical string // canonical re-serialization
	}
	cases := []rtCase{
		{
			in:        `{"amount":"123456","precision":3,"nai":"@@000000021"}`,
			wantAmt:   123456,
			wantDec:   3,
			wantSym:   HiveSymbol,
			canonical: `{"amount":"123456","precision":3,"nai":"@@000000021"}`,
		},
		{
			in:        `{"amount":"654321","precision":3,"nai":"@@000000013"}`,
			wantAmt:   654321,
			wantDec:   3,
			wantSym:   HbdSymbol,
			canonical: `{"amount":"654321","precision":3,"nai":"@@000000013"}`,
		},
		{
			in:        `{"amount":"123456789","precision":6,"nai":"@@000000037"}`,
			wantAmt:   123456789,
			wantDec:   6,
			wantSym:   VestsSymbol,
			canonical: `{"amount":"123456789","precision":6,"nai":"@@000000037"}`,
		},
		// Whitespace tolerance (C++ fc::json::from_string ignores spaces).
		{
			in:        `{"amount":"456",       "precision":3, "nai":"@@000000021"}`,
			wantAmt:   456,
			wantDec:   3,
			wantSym:   HiveSymbol,
			canonical: `{"amount":"456","precision":3,"nai":"@@000000021"}`,
		},
		{
			in:        `{"amount":"56", "precision":3, "nai":"@@000000021"}`,
			wantAmt:   56,
			wantDec:   3,
			wantSym:   HiveSymbol,
			canonical: `{"amount":"56","precision":3,"nai":"@@000000021"}`,
		},
	}
	for _, c := range cases {
		var a Asset
		if err := json.Unmarshal([]byte(c.in), &a); err != nil {
			t.Errorf("UnmarshalJSON(%s): %v", c.in, err)
			continue
		}
		if a.Amount != c.wantAmt {
			t.Errorf("UnmarshalJSON(%s) amount = %d, want %d", c.in, a.Amount, c.wantAmt)
		}
		if a.Symbol.Decimals() != c.wantDec {
			t.Errorf("UnmarshalJSON(%s) decimals = %d, want %d", c.in, a.Symbol.Decimals(), c.wantDec)
		}
		if a.Symbol.Num != c.wantSym {
			t.Errorf("UnmarshalJSON(%s) symbol = %#x, want %#x",
				c.in, uint32(a.Symbol.Num), uint32(c.wantSym))
		}
		out, err := json.Marshal(a)
		if err != nil {
			t.Errorf("MarshalJSON(%+v): %v", a, err)
			continue
		}
		if string(out) != c.canonical {
			t.Errorf("MarshalJSON: got %s, want %s", out, c.canonical)
		}
	}

	// Constructed assets also marshal to the well-known forms (matches
	// BOOST_CHECK_EQUAL( fc::json::to_string( asset( 50, HIVE_SYMBOL ) ), ...).
	constructed := []struct {
		a    Asset
		want string
	}{
		{Asset{50, Symbol{Num: HiveSymbol}}, `{"amount":"50","precision":3,"nai":"@@000000021"}`},
		{Asset{50000, Symbol{Num: HiveSymbol}}, `{"amount":"50000","precision":3,"nai":"@@000000021"}`},
		{Asset{50, Symbol{Num: HbdSymbol}}, `{"amount":"50","precision":3,"nai":"@@000000013"}`},
		{Asset{50000, Symbol{Num: HbdSymbol}}, `{"amount":"50000","precision":3,"nai":"@@000000013"}`},
		{Asset{50, Symbol{Num: VestsSymbol}}, `{"amount":"50","precision":6,"nai":"@@000000037"}`},
		{Asset{50000, Symbol{Num: VestsSymbol}}, `{"amount":"50000","precision":6,"nai":"@@000000037"}`},
	}
	for _, c := range constructed {
		got, err := json.Marshal(c.a)
		if err != nil {
			t.Errorf("MarshalJSON(%+v): %v", c.a, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("MarshalJSON(%+v) = %s, want %s", c.a, got, c.want)
		}
	}

	// Error cases from the C++ test: each must fail.
	failCases := []string{
		// amount overflow (> int64 max)
		`{"amount":"9223372036854775808","precision":3,"nai":"@@000000021"}`,
		// amount underflow (negative)
		`{"amount":"-1","precision":3,"nai":"@@000000021"}`,
		// precision overflow
		`{"amount":"10","precision":256,"nai":"@@000000021"}`,
		// precision underflow (negative)
		`{"amount":"10","precision":-1,"nai":"@@000000021"}`,
		// malformed JSON
		`{"amount":"0",3]`,
		// trailing junk after final brace
		`{"amount":"0","precision":3,"nai":"@@000000021",1}`,
		// non-numeric in amount
		`{"amount":"foobar","precision":3,"nai":"@@000000021"}`,
		`{"amount":"10a","precision":3,"nai":"@@000000021"}`,
		`{"amount":"10a00","precision":3,"nai":"@@000000021"}`,
		// hex value
		`{"amount":"0x8000","precision":3,"nai":"@@000000021"}`,
	}
	for _, in := range failCases {
		var a Asset
		if err := json.Unmarshal([]byte(in), &a); err == nil {
			t.Errorf("UnmarshalJSON(%s) accepted, expected error", in)
		}
	}

	// "Octal" leading-zero string is accepted as decimal (matches C++
	// boost::lexical_cast behaviour: BOOST_CHECK_EQUAL( ... 08000 ... , 8000 )).
	var oct Asset
	if err := json.Unmarshal([]byte(`{"amount":"08000","precision":3,"nai":"@@000000021"}`), &oct); err != nil {
		t.Errorf("UnmarshalJSON(08000): unexpected error %v", err)
	} else if oct.Amount != 8000 {
		t.Errorf("UnmarshalJSON(08000) amount = %d, want 8000", oct.Amount)
	}
}

// TestAssetSymbolNAIStringForm ports the from_nai_string portion of
// serialization_tests.cpp::asset_symbol_type_test.
func TestAssetSymbolNAIStringForm(t *testing.T) {
	// from_nai_string("@@100000006", 3) matches the SMT NAI form built
	// from `(10000000 << 5) | SMT_ASSET_NUM_CONTROL_MASK | 3`.
	const naiShift = 5
	const smtControlMask uint32 = 1 << 4
	wantAssetNum := AssetNum((uint32(10000000) << naiShift) | smtControlMask | 3)
	got, err := SymbolFromNAIString("@@100000006", 3)
	if err != nil {
		t.Fatalf("SymbolFromNAIString(@@100000006, 3): %v", err)
	}
	if got.Num != wantAssetNum {
		t.Errorf("SymbolFromNAIString(@@100000006, 3).Num = %#x, want %#x",
			uint32(got.Num), uint32(wantAssetNum))
	}
	if got.WasLegacyEncoded {
		t.Errorf("SymbolFromNAIString must not set WasLegacyEncoded")
	}

	// to_nai_string round-trip.
	if s := got.ToNAIString(); s != "@@100000006" {
		t.Errorf("ToNAIString(@@100000006) = %q, want %q", s, "@@100000006")
	}
	if n := got.ToNAI(); n != 100000006 {
		t.Errorf("ToNAI() = %d, want 100000006", n)
	}

	// Well-known symbols round-trip through their NAI strings.
	wellKnown := []struct {
		nai       string
		precision uint8
		want      AssetNum
	}{
		{"@@000000021", 3, HiveSymbol},
		{"@@000000013", 3, HbdSymbol},
		{"@@000000037", 6, VestsSymbol},
	}
	for _, c := range wellKnown {
		sym, err := SymbolFromNAIString(c.nai, c.precision)
		if err != nil {
			t.Errorf("SymbolFromNAIString(%s, %d): %v", c.nai, c.precision, err)
			continue
		}
		if sym.Num != c.want {
			t.Errorf("SymbolFromNAIString(%s, %d) = %#x, want %#x",
				c.nai, c.precision, uint32(sym.Num), uint32(c.want))
		}
		if s := (Symbol{Num: c.want}).ToNAIString(); s != c.nai {
			t.Errorf("ToNAIString(%#x) = %q, want %q", uint32(c.want), s, c.nai)
		}
	}

	// Error cases mirroring the C++ HIVE_REQUIRE_THROW lines.
	type errCase struct {
		name      string
		nai       string
		precision uint8
	}
	errs := []errCase{
		{"too many decimals", "@@100000006", hiveAssetMaxDecimals + 1},
		{"invalid NAI prefix", "@0100000006", 3},
		{"length too short", "@@00000006", 3},
		{"length too long", "@@0100000006", 3},
		{"non-digit body", "@@invalid00", 3},
		// Well-known NAIs reject mismatched precision.
		{"HIVE wrong precision", "@@000000021", 6},
		{"HBD wrong precision", "@@000000013", 6},
		{"VESTS wrong precision", "@@000000037", 3},
		// Invalid Damm check digit.
		{"bad check digit", "@@000000020", 3},
	}
	for _, c := range errs {
		if _, err := SymbolFromNAIString(c.nai, c.precision); err == nil {
			t.Errorf("%s: SymbolFromNAIString(%q, %d) accepted, expected error",
				c.name, c.nai, c.precision)
		}
	}

	// Empty string is also rejected (C++ rejects null/short strings).
	if _, err := SymbolFromNAIString("", 3); err == nil {
		t.Errorf("empty NAI string accepted")
	}
	// And the error message references the wrong-length condition.
	if _, err := SymbolFromNAIString("nope", 3); err == nil || !strings.Contains(err.Error(), "length") {
		t.Errorf("expected length error, got %v", err)
	}
}
