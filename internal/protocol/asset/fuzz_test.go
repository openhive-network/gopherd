package asset

import (
	"bytes"
	"testing"

	"gopherd/internal/fcraw"
)

// FuzzReadAssetAndPrice decodes a binary asset and a price, as every
// operation carrying an amount does. Neither may panic, and what they accept
// must re-encode to exactly the bytes consumed (assets have no flexible
// encodings: an int64 and an 8-byte symbol).
func FuzzReadAssetAndPrice(f *testing.F) {
	f.Add(AppendAsset(nil, Asset{Amount: 1000, Symbol: Symbol{Num: HiveSymbol}}))
	f.Add(AppendPrice(nil, Price{
		Base:  Asset{Amount: 1, Symbol: Symbol{Num: HbdSymbol}},
		Quote: Asset{Amount: 3, Symbol: Symbol{Num: HiveSymbol}},
	}))
	f.Fuzz(func(t *testing.T, src []byte) {
		r := fcraw.NewReader(src)
		if a, err := ReadAsset(r); err == nil {
			if enc := AppendAsset(nil, a); !bytes.Equal(enc, src[:r.Pos()]) {
				t.Fatalf("asset re-encodes differently\n in: %x\nout: %x", src[:r.Pos()], enc)
			}
		}
		r = fcraw.NewReader(src)
		if p, err := ReadPrice(r); err == nil {
			if enc := AppendPrice(nil, p); !bytes.Equal(enc, src[:r.Pos()]) {
				t.Fatalf("price re-encodes differently\n in: %x\nout: %x", src[:r.Pos()], enc)
			}
		}
	})
}

// FuzzParseLegacyAsset parses the "1.000 HIVE" form that API clients send.
// It must not panic, and an asset it accepts must print to a string that
// parses back to the same asset.
func FuzzParseLegacyAsset(f *testing.F) {
	for _, s := range []string{"1.000 HIVE", "0.001 HBD", "123.456789 VESTS", "1.000 STEEM", "-1.000 HIVE", "1e3 HIVE", ".5 HBD", "1.0000 HIVE"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a, err := ParseLegacyAsset(s)
		if err != nil {
			return
		}
		out := a.String()
		b, err := ParseLegacyAsset(out)
		if err != nil {
			t.Fatalf("%q parsed to %+v, which prints as %q and does not parse back: %v", s, a, out, err)
		}
		if b.Amount != a.Amount || b.Symbol.Num != a.Symbol.Num {
			t.Fatalf("%q -> %+v -> %q -> %+v", s, a, out, b)
		}
	})
}

// FuzzAssetJSON round-trips the JSON forms API clients send (legacy string
// and NAI object). Unmarshal must not panic, and an accepted asset must
// survive MarshalJSON/UnmarshalJSON unchanged.
func FuzzAssetJSON(f *testing.F) {
	for _, s := range []string{`"1.000 HIVE"`, `{"amount":"1000","precision":3,"nai":"@@000000021"}`, `{"amount":"1","precision":6,"nai":"@@000000037"}`, `null`, `{}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var a Asset
		if err := a.UnmarshalJSON(data); err != nil {
			return
		}
		out, err := a.MarshalJSON()
		if err != nil {
			t.Fatalf("%s accepted as %+v but MarshalJSON fails: %v", data, a, err)
		}
		var b Asset
		if err := b.UnmarshalJSON(out); err != nil {
			t.Fatalf("%s -> %s does not decode: %v", data, out, err)
		}
		if b.Amount != a.Amount || b.Symbol.Num != a.Symbol.Num {
			t.Fatalf("%s -> %+v -> %s -> %+v", data, a, out, b)
		}
	})
}
