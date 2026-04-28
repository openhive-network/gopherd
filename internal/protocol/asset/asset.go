package asset

import (
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
