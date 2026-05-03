package asset

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ParseLegacyAsset mirrors hive::protocol::legacy_asset::from_string from
// libraries/protocol/asset.cpp. Format is "<integer>.<frac> SYMBOL"
// with `frac` being exactly Symbol.Decimals() digits, no extra
// whitespace, and a recognized symbol name. On error returns a
// non-nil error matching the C++ throw conditions.
//
// Recognized symbols on mainnet: HIVE, HBD, VESTS. Recognized on
// testnet: TESTS, TBD, VESTS. Either set is accepted here — callers
// who need network-specific strictness can post-validate.
func ParseLegacyAsset(s string) (Asset, error) {
	if s == "" {
		return Asset{}, errors.New("asset: empty string")
	}
	// Reject leading/trailing whitespace and double-dots.
	if strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		return Asset{}, errors.New("asset: leading/trailing space")
	}
	parts := strings.Split(s, " ")
	if len(parts) != 2 {
		return Asset{}, fmt.Errorf("asset: expected '<amount> <symbol>', got %q", s)
	}
	amountStr, symStr := parts[0], parts[1]
	if amountStr == "" || symStr == "" {
		return Asset{}, errors.New("asset: empty amount or symbol")
	}

	// Resolve symbol → AssetNum and expected decimals.
	var sym Symbol
	switch symStr {
	case "HIVE", "TESTS":
		sym = Symbol{Num: HiveSymbol, WasLegacyEncoded: true}
	case "HBD", "TBD":
		sym = Symbol{Num: HbdSymbol, WasLegacyEncoded: true}
	case "VESTS", "VEST", "VESTS6":
		sym = Symbol{Num: VestsSymbol, WasLegacyEncoded: true}
	default:
		return Asset{}, fmt.Errorf("asset: unrecognized symbol %q", symStr)
	}
	wantDecimals := int(sym.Decimals())

	// Parse amount: must contain exactly one '.', followed by exactly
	// wantDecimals digits.
	dot := strings.IndexByte(amountStr, '.')
	if dot < 0 {
		return Asset{}, fmt.Errorf("asset: missing decimal point in %q", amountStr)
	}
	if strings.Count(amountStr, ".") != 1 {
		return Asset{}, fmt.Errorf("asset: multiple decimal points in %q", amountStr)
	}
	whole := amountStr[:dot]
	frac := amountStr[dot+1:]
	if len(frac) != wantDecimals {
		return Asset{}, fmt.Errorf("asset: %q expected %d decimal places, got %d",
			amountStr, wantDecimals, len(frac))
	}
	// Detect spurious whitespace inside the amount.
	if strings.Contains(amountStr, " ") {
		return Asset{}, fmt.Errorf("asset: whitespace inside amount %q", amountStr)
	}
	// Whole portion may be signed.
	if whole == "" || whole == "-" {
		return Asset{}, fmt.Errorf("asset: empty whole portion in %q", amountStr)
	}
	// Validate digits.
	for i, r := range whole {
		if i == 0 && r == '-' {
			continue
		}
		if r < '0' || r > '9' {
			return Asset{}, fmt.Errorf("asset: non-digit in whole %q", whole)
		}
	}
	for _, r := range frac {
		if r < '0' || r > '9' {
			return Asset{}, fmt.Errorf("asset: non-digit in frac %q", frac)
		}
	}

	// Combined as integer of "whole" * 10^decimals + "frac".
	combined := whole + frac
	amt, err := strconv.ParseInt(combined, 10, 64)
	if err != nil {
		return Asset{}, fmt.Errorf("asset: amount overflow %q: %w", amountStr, err)
	}
	return Asset{Amount: amt, Symbol: sym}, nil
}
