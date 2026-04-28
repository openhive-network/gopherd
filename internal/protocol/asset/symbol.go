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
