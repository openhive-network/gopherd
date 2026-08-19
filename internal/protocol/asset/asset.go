package asset

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strconv"

	"gopherd/internal/fcraw"
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

// MarshalJSON renders a in the HF26 object form used by the C++
// `to_variant( asset )` helper:
//
//	{"amount":"<int64 as string>","precision":<uint8>,"nai":"@@NNNNNNNNN"}
//
// Reference: libraries/protocol/asset.cpp::to_variant(asset).
func (a Asset) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"amount":"`)
	buf.WriteString(strconv.FormatInt(a.Amount, 10))
	buf.WriteString(`","precision":`)
	buf.WriteString(strconv.FormatUint(uint64(a.Symbol.Decimals()), 10))
	buf.WriteString(`,"nai":"`)
	buf.WriteString(a.Symbol.ToNAIString())
	buf.WriteString(`"}`)
	return buf.Bytes(), nil
}

// UnmarshalJSON parses the HF26 object form. Mirrors
// libraries/protocol/asset.cpp::from_variant(asset): amount must be a string
// holding a non-negative int64, precision must be in [0, HIVE_ASSET_MAX_DECIMALS]
// (matched by from_nai_string for well-known NAIs), and nai must be an
// "@@NNNNNNNNN" string with a valid Damm check digit. Hex amounts are
// rejected; octal-like leading zeros are accepted as decimal (matching C++
// boost::lexical_cast behaviour seen in the test suite).
func (a *Asset) UnmarshalJSON(data []byte) error {
	var raw struct {
		Amount    *json.RawMessage `json:"amount"`
		Precision *json.RawMessage `json:"precision"`
		NAI       *json.RawMessage `json:"nai"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	// Reject duplicate or unknown fields to match the C++ FC_ASSERTs that
	// reject extra/missing keys.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("asset: invalid JSON object: %w", err)
	}
	if dec.More() {
		return fmt.Errorf("asset: trailing JSON tokens after object")
	}
	if raw.Amount == nil {
		return fmt.Errorf("asset: missing %q field", "amount")
	}
	if raw.Precision == nil {
		return fmt.Errorf("asset: missing %q field", "precision")
	}
	if raw.NAI == nil {
		return fmt.Errorf("asset: missing %q field", "nai")
	}

	// amount: must be a JSON string holding a non-negative int64.
	var amountStr string
	if err := json.Unmarshal(*raw.Amount, &amountStr); err != nil {
		return fmt.Errorf("asset: amount must be a string: %w", err)
	}
	if amountStr == "" {
		return fmt.Errorf("asset: empty amount string")
	}
	// Reject hex (boost::lexical_cast<int64_t> rejects "0x..."). Plain
	// digits only — leading '0's are allowed and treated as decimal.
	for i, c := range amountStr {
		if i == 0 && c == '-' {
			// boost::lexical_cast accepts a leading '-' but the C++ test
			// requires amount >= 0; surface as an error below after parsing.
			continue
		}
		if c < '0' || c > '9' {
			return fmt.Errorf("asset: non-numeric character %q in amount %q", c, amountStr)
		}
	}
	amt, err := strconv.ParseInt(amountStr, 10, 64)
	if err != nil {
		return fmt.Errorf("asset: amount %q out of int64 range: %w", amountStr, err)
	}
	if amt < 0 {
		return fmt.Errorf("asset: amount %d cannot be negative", amt)
	}

	// precision: JSON number → uint8, must be <= HIVE_ASSET_MAX_DECIMALS.
	var precision uint64
	if err := json.Unmarshal(*raw.Precision, &precision); err != nil {
		return fmt.Errorf("asset: precision must be an unsigned integer: %w", err)
	}
	if precision > hiveAssetMaxDecimals {
		return fmt.Errorf("asset: precision %d exceeds max %d", precision, hiveAssetMaxDecimals)
	}

	// nai: "@@NNNNNNNNN".
	var naiStr string
	if err := json.Unmarshal(*raw.NAI, &naiStr); err != nil {
		return fmt.Errorf("asset: nai must be a string: %w", err)
	}
	sym, err := SymbolFromNAIString(naiStr, uint8(precision))
	if err != nil {
		return err
	}
	a.Amount = amt
	a.Symbol = sym
	return nil
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
	legacyHiveSymCanonical = legacySymBytes(3, legacyHiveName)
	legacyHiveSym1         = legacySymBytes(1, legacyHiveName)
	legacyHiveSym2         = legacySymBytes(2, legacyHiveName)
	legacyHiveSym3         = legacySymBytes(5, legacyHiveName)
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
