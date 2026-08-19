package asset

import (
	"bytes"
	"testing"

	"gopherd/internal/fcraw"
)

func TestSymbolConstants(t *testing.T) {
	// Confirm asset_num values match the C++ HIVE_ASSET_NUM_* macros.
	// HIVE_ASSET_NUM_HIVE = ((100_000_001) << 5) | 3 = 0xBEBC2023
	if uint32(HiveSymbol) != 0xBEBC2023 {
		t.Errorf("HiveSymbol asset_num: got %#x, want 0xBEBC2023", uint32(HiveSymbol))
	}
	// HBD = ((100_000_000) << 5) | 3 = 0xBEBC2003
	if uint32(HbdSymbol) != 0xBEBC2003 {
		t.Errorf("HbdSymbol asset_num: got %#x, want 0xBEBC2003", uint32(HbdSymbol))
	}
	// VESTS = ((100_000_002) << 5) | 6 = 0xBEBC2046
	if uint32(VestsSymbol) != 0xBEBC2046 {
		t.Errorf("VestsSymbol asset_num: got %#x, want 0xBEBC2046", uint32(VestsSymbol))
	}
}

func TestSymbolDecimals(t *testing.T) {
	cases := []struct {
		s    Symbol
		want uint8
	}{
		{Symbol{Num: HiveSymbol}, 3},
		{Symbol{Num: HbdSymbol}, 3},
		{Symbol{Num: VestsSymbol}, 6},
	}
	for _, c := range cases {
		if got := c.s.Decimals(); got != c.want {
			t.Errorf("%s decimals: got %d, want %d", c.s.String(), got, c.want)
		}
	}
}

func TestLegacySymbolDecode(t *testing.T) {
	cases := []struct {
		name    string
		bytes   []byte
		wantNum AssetNum
	}{
		// Legacy names are per-network: STEEM/SBD on mainnet, TESTS/TBD on a
		// testnet build. Built from the same source as the encoder so this
		// asserts the ACTIVE network's wire format rather than mainnet's.
		{"HIVE (legacy " + legacyHiveName + ")", legacySymBytes(3, legacyHiveName), HiveSymbol},
		{"HBD (legacy " + legacyHbdName + ")", legacySymBytes(3, legacyHbdName), HbdSymbol},
		{"VESTS", []byte{0x06, 'V', 'E', 'S', 'T', 'S', 0x00, 0x00}, VestsSymbol},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := fcraw.NewReader(c.bytes)
			s, err := ReadSymbol(r)
			if err != nil {
				t.Fatal(err)
			}
			if s.Num != c.wantNum {
				t.Errorf("got %#x, want %#x", uint32(s.Num), uint32(c.wantNum))
			}
			if !s.WasLegacyEncoded {
				t.Error("WasLegacyEncoded should be true")
			}
			if r.Remaining() != 0 {
				t.Errorf("expected reader at EOF, %d bytes remain", r.Remaining())
			}
			// Round-trip back to bytes using the preserved encoding.
			out := AppendSymbol(nil, s)
			if !bytes.Equal(out, c.bytes) {
				t.Errorf("round-trip mismatch:\n got %x\nwant %x", out, c.bytes)
			}
		})
	}
}

func TestNAISymbolDecode(t *testing.T) {
	// 4-byte NAI form for HIVE_ASSET_NUM_HIVE = 0xBEBC2023.
	src := []byte{0x23, 0x20, 0xBC, 0xBE}
	r := fcraw.NewReader(src)
	s, err := ReadSymbol(r)
	if err != nil {
		t.Fatal(err)
	}
	if s.Num != HiveSymbol {
		t.Errorf("got %#x, want %#x", uint32(s.Num), uint32(HiveSymbol))
	}
	if s.WasLegacyEncoded {
		t.Error("NAI-decoded symbol must not be flagged WasLegacyEncoded")
	}
	if r.Remaining() != 0 {
		t.Errorf("expected reader at EOF, %d bytes remain", r.Remaining())
	}
	// Round-trip via the NAI path.
	out := AppendSymbol(nil, s)
	if !bytes.Equal(out, src) {
		t.Errorf("round-trip mismatch:\n got %x\nwant %x", out, src)
	}
}

func TestAssetRoundTrip(t *testing.T) {
	cases := []Asset{
		{Amount: 1234, Symbol: Symbol{Num: HiveSymbol, WasLegacyEncoded: true}},
		{Amount: -42, Symbol: Symbol{Num: HbdSymbol, WasLegacyEncoded: true}},
		{Amount: 1_000_000, Symbol: Symbol{Num: VestsSymbol, WasLegacyEncoded: true}},
		{Amount: 0, Symbol: Symbol{Num: HiveSymbol}}, // NAI form
	}
	for _, a := range cases {
		t.Run(a.String(), func(t *testing.T) {
			wire := AppendAsset(nil, a)
			r := fcraw.NewReader(wire)
			got, err := ReadAsset(r)
			if err != nil {
				t.Fatal(err)
			}
			if got.Amount != a.Amount {
				t.Errorf("amount: got %d, want %d", got.Amount, a.Amount)
			}
			if got.Symbol.Num != a.Symbol.Num {
				t.Errorf("symbol: got %#x, want %#x", uint32(got.Symbol.Num), uint32(a.Symbol.Num))
			}
			if r.Remaining() != 0 {
				t.Errorf("trailing bytes: %d", r.Remaining())
			}
		})
	}
}

func TestAssetString(t *testing.T) {
	cases := []struct {
		a    Asset
		want string
	}{
		{Asset{1234, Symbol{Num: HiveSymbol}}, "1.234 HIVE"},
		{Asset{42_500, Symbol{Num: HbdSymbol}}, "42.500 HBD"},
		{Asset{1_500_000, Symbol{Num: VestsSymbol}}, "1.500000 VESTS"},
		{Asset{-1500, Symbol{Num: HiveSymbol}}, "-1.500 HIVE"},
		{Asset{1, Symbol{Num: HiveSymbol}}, "0.001 HIVE"},
	}
	for _, c := range cases {
		if got := c.a.String(); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

func TestPriceRoundTrip(t *testing.T) {
	p := Price{
		Base:  Asset{Amount: 1, Symbol: Symbol{Num: HbdSymbol, WasLegacyEncoded: true}},
		Quote: Asset{Amount: 4, Symbol: Symbol{Num: HiveSymbol, WasLegacyEncoded: true}},
	}
	wire := AppendPrice(nil, p)
	got, err := ReadPrice(fcraw.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	if got.Base.Amount != p.Base.Amount || got.Base.Symbol.Num != p.Base.Symbol.Num {
		t.Errorf("base mismatch: %+v vs %+v", got.Base, p.Base)
	}
	if got.Quote.Amount != p.Quote.Amount || got.Quote.Symbol.Num != p.Quote.Symbol.Num {
		t.Errorf("quote mismatch: %+v vs %+v", got.Quote, p.Quote)
	}
}
