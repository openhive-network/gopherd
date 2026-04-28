package asset

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"hived-go/internal/fcraw"
)

// Asset is the wire form of `hive::protocol::asset` — an int64 amount paired
// with an asset_symbol_type. On the wire it is the amount (8 bytes LE)
// followed by the symbol (4 or 8 bytes per Symbol's encoding rules).
type Asset struct {
	Amount int64
	Symbol Symbol
}

// String renders the asset in human-readable form (e.g. "1.234 HIVE").
func (a Asset) String() string {
	d := int(a.Symbol.Decimals())
	if d == 0 {
		return fmt.Sprintf("%d %s", a.Amount, a.Symbol.String())
	}
	// Build a fixed-precision decimal without floating point so values near
	// int64 limits stay exact.
	neg := a.Amount < 0
	abs := a.Amount
	if neg {
		abs = -abs
	}
	div := int64(1)
	for i := 0; i < d; i++ {
		div *= 10
	}
	whole := abs / div
	frac := abs % div
	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%0*d %s", sign, whole, d, frac, a.Symbol.String())
}

// ReadAsset reads amount (8 bytes LE) then a Symbol from r.
func ReadAsset(r *fcraw.Reader) (Asset, error) {
	amount, err := r.ReadInt64()
	if err != nil {
		return Asset{}, fmt.Errorf("asset.ReadAsset amount: %w", err)
	}
	sym, err := ReadSymbol(r)
	if err != nil {
		return Asset{}, fmt.Errorf("asset.ReadAsset symbol: %w", err)
	}
	return Asset{Amount: amount, Symbol: sym}, nil
}

// AppendAsset writes a to dst in its preserved encoding.
func AppendAsset(dst []byte, a Asset) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, uint64(a.Amount))
	return AppendSymbol(dst, a.Symbol)
}

// Price is the on-wire form of hive::protocol::price — two assets, the base
// and the quote, with no length prefix between them.
type Price struct {
	Base  Asset
	Quote Asset
}

// ReadPrice reads two consecutive assets.
func ReadPrice(r *fcraw.Reader) (Price, error) {
	base, err := ReadAsset(r)
	if err != nil {
		return Price{}, fmt.Errorf("asset.ReadPrice base: %w", err)
	}
	quote, err := ReadAsset(r)
	if err != nil {
		return Price{}, fmt.Errorf("asset.ReadPrice quote: %w", err)
	}
	return Price{Base: base, Quote: quote}, nil
}

// AppendPrice writes p to dst (base then quote, no separator).
func AppendPrice(dst []byte, p Price) []byte {
	dst = AppendAsset(dst, p.Base)
	return AppendAsset(dst, p.Quote)
}

// LegacyHiveAsset is hive::protocol::legacy_hive_asset — the historical
// "amount + 8-byte legacy HIVE symbol" form used by witness_update.props
// (account_creation_fee). Unlike a regular asset, the symbol can be one of
// six accepted byte sequences:
//
//	canonical "STEEM\0\0" with precision 3                      (OBSOLETE_SYMBOL_SER)
//	"STEEM\0\0" with precision 1                                (LEGACY_SER_1)
//	"STEEM\0\0" with precision 2                                (LEGACY_SER_2)
//	"STEEM\0\0" with precision 5                                (LEGACY_SER_3)
//	"0.001\0\0" with precision 3                                (LEGACY_SER_4)
//	"6.000\0\0" with precision 3                                (LEGACY_SER_5)
//
// or, post-HF26 / hf26 pack mode, a 4-byte HIVE_ASSET_NUM (0xBEBC2023).
//
// Reference: libraries/protocol/include/hive/protocol/legacy_asset.hpp.
// Witnesses on these blocks set non-canonical fees; consensus accepts them
// and we must round-trip the original byte sequence.
type LegacyHiveAsset struct {
	Amount int64
	// Sym is the raw 8-byte legacy frame, OR a 4-byte NAI frame. We
	// preserve the wire bytes so that re-marshalling is bit-identical.
	// Length is always 4 or 8.
	Sym []byte
}

// canonical legacy and non-canonical accepted serializations (8 bytes each).
var (
	legacyHiveSymCanonical = []byte{0x03, 'S', 'T', 'E', 'E', 'M', 0x00, 0x00}
	legacyHiveSym1         = []byte{0x01, 'S', 'T', 'E', 'E', 'M', 0x00, 0x00}
	legacyHiveSym2         = []byte{0x02, 'S', 'T', 'E', 'E', 'M', 0x00, 0x00}
	legacyHiveSym3         = []byte{0x05, 'S', 'T', 'E', 'E', 'M', 0x00, 0x00}
	legacyHiveSym4         = []byte{0x03, '0', '.', '0', '0', '1', 0x00, 0x00}
	legacyHiveSym5         = []byte{0x03, '6', '.', '0', '0', '0', 0x00, 0x00}
)

// ReadLegacyHiveAsset reads the (int64 amount, legacy_hive_asset_symbol)
// 12- or 16-byte structure used by witness_update.props.
//
//	bytes 0..7   : int64 amount LE
//	bytes 8..11  : first 4 bytes of symbol
//	  if those 4 bytes equal HIVE_ASSET_NUM_HIVE (4-byte NAI), done at 12.
//	  otherwise read 4 more bytes (16 total). Validate symbol against the
//	  six accepted values.
func ReadLegacyHiveAsset(r *fcraw.Reader) (LegacyHiveAsset, error) {
	var a LegacyHiveAsset
	amount, err := r.ReadInt64()
	if err != nil {
		return a, fmt.Errorf("legacy_hive_asset.amount: %w", err)
	}
	a.Amount = amount

	first4 := make([]byte, 4)
	if err := r.ReadInto(first4); err != nil {
		return a, fmt.Errorf("legacy_hive_asset.symbol[0..4]: %w", err)
	}
	if binary.LittleEndian.Uint32(first4) == uint32(HiveSymbol) {
		a.Sym = first4
		return a, nil
	}
	tail := make([]byte, 4)
	if err := r.ReadInto(tail); err != nil {
		return a, fmt.Errorf("legacy_hive_asset.symbol[4..8]: %w", err)
	}
	full := append(first4, tail...)
	switch {
	case bytes.Equal(full, legacyHiveSymCanonical),
		bytes.Equal(full, legacyHiveSym1),
		bytes.Equal(full, legacyHiveSym2),
		bytes.Equal(full, legacyHiveSym3),
		bytes.Equal(full, legacyHiveSym4),
		bytes.Equal(full, legacyHiveSym5):
		a.Sym = full
		return a, nil
	}
	return a, fmt.Errorf("legacy_hive_asset: unexpected symbol bytes %x", full)
}

// AppendLegacyHiveAsset writes a back to dst preserving the on-wire symbol
// bytes captured during decode.
func AppendLegacyHiveAsset(dst []byte, a LegacyHiveAsset) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, uint64(a.Amount))
	return append(dst, a.Sym...)
}
