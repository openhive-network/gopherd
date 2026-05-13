// Tests ported from hive/tests/unit/tests/serialization_tests.cpp covering
// asset / asset_symbol_type binary and bit-layout behavior. JSON-variant
// cases that rely on fc::variant for asset are skipped (Go uses
// encoding/json, not fc::json, and asset has no encoding/json codec by
// design — wire form is canonical).
package asset

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"hived-go/internal/fcraw"
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
// "nai":"@@000000021"}`. Go's asset has no encoding/json marshaller — the
// wire form is canonical and JSON exposure lives in plugins/api (NAI-string
// form for asset_symbol_type, plus the {amount,precision,nai} object).
// Skip until the JSON codec lands.
func TestAssetJSONForm(t *testing.T) {
	t.Skip("MISSING: asset encoding/json marshaller ({amount,precision,nai} form)")
}

// TestAssetSymbolNAIStringForm ports the from_nai_string portion of
// serialization_tests.cpp::asset_symbol_type_test. Go has no NAI-string
// parser (the wire form is the canonical exchange path).
func TestAssetSymbolNAIStringForm(t *testing.T) {
	t.Skip("MISSING: asset_symbol_type::from_nai_string parser")
}
