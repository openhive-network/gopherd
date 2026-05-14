// Package asset implements Hive's asset types — asset_symbol_type, asset,
// and price — and their FC binary codecs. Two on-the-wire encodings exist:
//
//	Legacy (pre-HF26 / serialization_mode legacy): 8 bytes for the well-known
//	  assets HIVE/HBD/VESTS — 1 byte precision followed by the asset's 5- or
//	  3-character symbol name (NUL-padded to 7 bytes). Note that legacy HIVE
//	  is encoded as "STEEM" and legacy HBD as "SBD", reflecting the chain's
//	  fork ancestry.
//	NAI (post-HF26 / hf26 mode): 4 bytes — asset_num as uint32 LE, packing
//	  `(NAI << 5) | precision`.
//
// Both encodings can appear in any block_log; which one was written depends
// on the producer node's serialization mode at write time. Decoders must
// auto-detect by inspecting the first 4 bytes:
//
//	0x4554_5303  → legacy HIVE  (low 4 bytes of "STEEM"|<<8 | 3)
//	0x4442_5303  → legacy HBD   ("SBD"|<<8 | 3)
//	0x5345_5606  → legacy VESTS ("VESTS"|<<8 | 6)
//	otherwise    → NAI asset_num
//
// Source: libraries/protocol/include/hive/protocol/asset_symbol.hpp
package asset

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
)

// AssetNum is the canonical internal representation of an asset symbol — a
// 32-bit packed value where the low bit(s) hold precision/control and the
// upper bits hold the NAI. Mirrors `asset_symbol_type::asset_num`.
type AssetNum uint32

// Well-known mainnet asset numbers, derived from the SMT NAI scheme:
//
//	HIVE_ASSET_NUM_HIVE  = ((SMT_MAX_NAI + 2) << 5) | 3
//	HIVE_ASSET_NUM_HBD   = ((SMT_MAX_NAI + 1) << 5) | 3
//	HIVE_ASSET_NUM_VESTS = ((SMT_MAX_NAI + 3) << 5) | 6
//
// where SMT_MAX_NAI = 99_999_999 and HIVE_NAI_SHIFT = 5.
const (
	smtMaxNAI    uint32 = 99_999_999
	hiveNAIShift        = 5

	HiveSymbol  AssetNum = AssetNum(((smtMaxNAI + 2) << hiveNAIShift) | 3)
	HbdSymbol   AssetNum = AssetNum(((smtMaxNAI + 1) << hiveNAIShift) | 3)
	VestsSymbol AssetNum = AssetNum(((smtMaxNAI + 3) << hiveNAIShift) | 6)
)

// Legacy 4-byte detection magic (low 32 bits of the 8-byte serialization).
// Any 4-byte read matching one of these means a legacy asset follows; the
// caller must consume 4 more bytes to complete the 8-byte legacy frame.
const (
	legacyHiveLow4  uint32 = 0x4554_5303 // 0x03 'S' 'T' 'E' (low) of "STEEM\0\0" + 3
	legacyHbdLow4   uint32 = 0x4442_5303 // 0x03 'S' 'B' 'D'
	legacyVestsLow4 uint32 = 0x5345_5606 // 0x06 'V' 'E' 'S'
)

// Symbol is an asset_symbol_type — either a well-known legacy symbol
// (HIVE/HBD/VESTS) or an SMT NAI symbol.
type Symbol struct {
	// Num is the canonical asset_num. For legacy assets this is one of the
	// HiveSymbol/HbdSymbol/VestsSymbol constants; for NAI symbols it's
	// (NAI << 5) | precision.
	Num AssetNum
	// WasLegacyEncoded preserves whether this Symbol was decoded from the
	// 8-byte legacy form. Round-trip preservation is required to match tx_id
	// bytes for blocks written in legacy mode. NAI-decoded symbols have
	// this false.
	WasLegacyEncoded bool
}

// Decimals returns the precision (lower 4 bits of asset_num).
func (s Symbol) Decimals() uint8 { return uint8(s.Num & 0xf) }

// String returns a human-readable representation suitable for logs.
func (s Symbol) String() string {
	switch s.Num {
	case HiveSymbol:
		return "HIVE"
	case HbdSymbol:
		return "HBD"
	case VestsSymbol:
		return "VESTS"
	default:
		return fmt.Sprintf("@@%d", uint32(s.Num)>>hiveNAIShift)
	}
}

// ReadSymbol consumes either 4 (NAI) or 8 (legacy) bytes from r and returns
// the parsed symbol. The reader's cursor is advanced by exactly the number
// of bytes consumed on the wire — useful when downstream callers also need
// to know an asset's byte length (e.g. to compute tx_ids).
func ReadSymbol(r *fcraw.Reader) (Symbol, error) {
	if r.Remaining() < 4 {
		return Symbol{}, fmt.Errorf("asset.ReadSymbol: need 4 bytes, have %d", r.Remaining())
	}
	low4, err := r.ReadUint32()
	if err != nil {
		return Symbol{}, err
	}
	switch low4 {
	case legacyHiveLow4:
		// Consume the remaining 4 bytes of the 8-byte legacy frame.
		if _, err := r.ReadUint32(); err != nil {
			return Symbol{}, fmt.Errorf("asset.ReadSymbol HIVE tail: %w", err)
		}
		return Symbol{Num: HiveSymbol, WasLegacyEncoded: true}, nil
	case legacyHbdLow4:
		if _, err := r.ReadUint32(); err != nil {
			return Symbol{}, fmt.Errorf("asset.ReadSymbol HBD tail: %w", err)
		}
		return Symbol{Num: HbdSymbol, WasLegacyEncoded: true}, nil
	case legacyVestsLow4:
		if _, err := r.ReadUint32(); err != nil {
			return Symbol{}, fmt.Errorf("asset.ReadSymbol VESTS tail: %w", err)
		}
		return Symbol{Num: VestsSymbol, WasLegacyEncoded: true}, nil
	default:
		return Symbol{Num: AssetNum(low4)}, nil
	}
}

// AppendSymbol writes the symbol to dst in its preserved encoding (legacy
// 8-byte for well-known assets that were originally legacy-decoded, 4-byte
// NAI otherwise). When constructing fresh symbols from typed code paths,
// MarshalLegacy / MarshalNAI are usually a clearer choice.
func AppendSymbol(dst []byte, s Symbol) []byte {
	if s.WasLegacyEncoded {
		switch s.Num {
		case HiveSymbol:
			return append(dst, 0x03, 'S', 'T', 'E', 'E', 'M', 0x00, 0x00)
		case HbdSymbol:
			return append(dst, 0x03, 'S', 'B', 'D', 0x00, 0x00, 0x00, 0x00)
		case VestsSymbol:
			return append(dst, 0x06, 'V', 'E', 'S', 'T', 'S', 0x00, 0x00)
		}
	}
	return appendU32LE(dst, uint32(s.Num))
}

// MarshalLegacy serializes s in the 8-byte legacy form. Errors if s is not a
// well-known symbol. Used by transaction round-trip code that must reproduce
// the exact bytes hived wrote pre-HF26.
func MarshalLegacy(dst []byte, s Symbol) ([]byte, error) {
	switch s.Num {
	case HiveSymbol:
		return append(dst, 0x03, 'S', 'T', 'E', 'E', 'M', 0x00, 0x00), nil
	case HbdSymbol:
		return append(dst, 0x03, 'S', 'B', 'D', 0x00, 0x00, 0x00, 0x00), nil
	case VestsSymbol:
		return append(dst, 0x06, 'V', 'E', 'S', 'T', 'S', 0x00, 0x00), nil
	}
	return nil, fmt.Errorf("asset.MarshalLegacy: %s has no legacy encoding", s.String())
}

// MarshalNAI serializes s in the 4-byte NAI form (HF26+).
func MarshalNAI(dst []byte, s Symbol) []byte { return appendU32LE(dst, uint32(s.Num)) }

func appendU32LE(dst []byte, v uint32) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// ErrInvalidLegacyFrame is returned when a 4-byte read suggests a legacy
// asset but the trailing 4 bytes of the 8-byte frame are inconsistent.
var ErrInvalidLegacyFrame = errors.New("asset: malformed legacy symbol frame")

// hiveAssetMaxDecimals mirrors HIVE_ASSET_MAX_DECIMALS.
const hiveAssetMaxDecimals = 12

// smtAssetNumControlMask mirrors SMT_ASSET_NUM_CONTROL_MASK (bit 4).
const smtAssetNumControlMask uint32 = 0x10

// Well-known NAI data digits (the 8-digit pre-checksum form used by
// HIVE/HBD/VESTS). Mirrors HIVE_NAI_HIVE/HIVE_NAI_HBD/HIVE_NAI_VESTS.
const (
	hiveNAIHbd   uint32 = 1
	hiveNAIHive  uint32 = 2
	hiveNAIVests uint32 = 3
)

// hiveNAIStringLength is HIVE_ASSET_SYMBOL_NAI_STRING_LENGTH - 1, i.e. the
// expected strlen of a NAI string: "@@" + 9 base digits = 11 chars.
const hiveNAIStringLength = 11

// dammChecksum8Digit computes the Damm-algorithm check digit for an 8-digit
// decimal value. Mirrors asset_symbol_type::damm_checksum_8digit. The input
// must be < 1e8.
func dammChecksum8Digit(value uint32) uint8 {
	t := [...]uint8{
		0, 30, 10, 70, 50, 90, 80, 60, 40, 20,
		70, 0, 90, 20, 10, 50, 40, 80, 60, 30,
		40, 20, 0, 60, 80, 70, 10, 30, 50, 90,
		10, 70, 50, 0, 90, 80, 30, 40, 20, 60,
		60, 10, 20, 30, 0, 40, 50, 90, 70, 80,
		30, 60, 70, 40, 20, 0, 90, 50, 80, 10,
		50, 80, 60, 90, 70, 20, 0, 10, 30, 40,
		80, 90, 40, 50, 30, 60, 20, 0, 10, 70,
		90, 40, 30, 80, 60, 10, 70, 20, 0, 50,
		20, 50, 80, 10, 40, 30, 60, 70, 90, 0,
	}
	q0 := value / 10
	d0 := value % 10
	q1 := q0 / 10
	d1 := q0 % 10
	q2 := q1 / 10
	d2 := q1 % 10
	q3 := q2 / 10
	d3 := q2 % 10
	q4 := q3 / 10
	d4 := q3 % 10
	q5 := q4 / 10
	d5 := q4 % 10
	d6 := q5 % 10
	d7 := q5 / 10

	x := t[d7]
	x = t[uint32(x)+d6]
	x = t[uint32(x)+d5]
	x = t[uint32(x)+d4]
	x = t[uint32(x)+d3]
	x = t[uint32(x)+d2]
	x = t[uint32(x)+d1]
	x = t[uint32(x)+d0]
	return x / 10
}

// assetNumFromNAI mirrors asset_symbol_type::asset_num_from_nai. The input is
// the 9-digit NAI (with check digit); the function validates the check digit
// and returns the canonical asset_num. Well-known NAIs (HIVE/HBD/VESTS) must
// be paired with their canonical precision.
func assetNumFromNAI(nai uint32, decimalPlaces uint8) (AssetNum, error) {
	naiCheckDigit := nai % 10
	naiDataDigits := nai / 10

	if naiDataDigits < 1 || naiDataDigits > smtMaxNAI {
		return 0, fmt.Errorf("asset: NAI %d out of range", nai)
	}
	if uint32(dammChecksum8Digit(naiDataDigits)) != naiCheckDigit {
		return 0, fmt.Errorf("asset: invalid NAI check digit in %d", nai)
	}

	switch naiDataDigits {
	case hiveNAIHive:
		if decimalPlaces != 3 {
			return 0, fmt.Errorf("asset: HIVE NAI requires precision 3, got %d", decimalPlaces)
		}
		return HiveSymbol, nil
	case hiveNAIHbd:
		if decimalPlaces != 3 {
			return 0, fmt.Errorf("asset: HBD NAI requires precision 3, got %d", decimalPlaces)
		}
		return HbdSymbol, nil
	case hiveNAIVests:
		if decimalPlaces != 6 {
			return 0, fmt.Errorf("asset: VESTS NAI requires precision 6, got %d", decimalPlaces)
		}
		return VestsSymbol, nil
	default:
		if decimalPlaces > hiveAssetMaxDecimals {
			return 0, fmt.Errorf("asset: precision %d exceeds max %d",
				decimalPlaces, hiveAssetMaxDecimals)
		}
		return AssetNum((naiDataDigits << hiveNAIShift) | smtAssetNumControlMask | uint32(decimalPlaces)), nil
	}
}

// SymbolFromNAIString parses a NAI-string of the form "@@NNNNNNNNN" (exactly
// "@@" followed by 9 decimal digits, including the Damm check digit) paired
// with a precision. Mirrors asset_symbol_type::from_nai_string. The well-known
// NAIs are HIVE="@@000000021", HBD="@@000000013", VESTS="@@000000037".
func SymbolFromNAIString(s string, decimalPlaces uint8) (Symbol, error) {
	if len(s) != hiveNAIStringLength {
		return Symbol{}, fmt.Errorf("asset: NAI string %q wrong length (got %d, want %d)",
			s, len(s), hiveNAIStringLength)
	}
	if s[0] != '@' || s[1] != '@' {
		return Symbol{}, fmt.Errorf("asset: NAI string %q missing '@@' prefix", s)
	}
	var nai uint32
	for i := 2; i < hiveNAIStringLength; i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return Symbol{}, fmt.Errorf("asset: NAI string %q non-digit at %d", s, i)
		}
		nai = nai*10 + uint32(c-'0')
	}
	num, err := assetNumFromNAI(nai, decimalPlaces)
	if err != nil {
		return Symbol{}, err
	}
	return Symbol{Num: num}, nil
}

// ToNAI returns the 9-digit NAI (data digits + Damm check digit) for s.
// Mirrors asset_symbol_type::to_nai. For HIVE/HBD/VESTS this returns
// 21/13/37 respectively; for SMT symbols it returns
// (asset_num >> HIVE_NAI_SHIFT) * 10 + damm_checksum.
func (s Symbol) ToNAI() uint32 {
	var dataDigits uint32
	switch s.Num {
	case HiveSymbol:
		dataDigits = hiveNAIHive
	case HbdSymbol:
		dataDigits = hiveNAIHbd
	case VestsSymbol:
		dataDigits = hiveNAIVests
	default:
		dataDigits = uint32(s.Num) >> hiveNAIShift
	}
	return dataDigits*10 + uint32(dammChecksum8Digit(dataDigits))
}

// ToNAIString returns the canonical "@@NNNNNNNNN" representation. Mirrors
// asset_symbol_type::to_nai_string.
func (s Symbol) ToNAIString() string {
	x := s.ToNAI()
	var buf [hiveNAIStringLength]byte
	buf[0] = '@'
	buf[1] = '@'
	for i := hiveNAIStringLength - 1; i >= 2; i-- {
		buf[i] = byte('0' + x%10)
		x /= 10
	}
	return string(buf[:])
}
