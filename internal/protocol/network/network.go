// Package network exposes the compile-time-selected HIVE_* consensus constants.
//
// The default build is mainnet. To select testnet, build with `-tags testnet`.
// Mirrornet is intentionally not supported (out of scope per the project plan).
//
// Source of truth: libraries/protocol/include/hive/protocol/config.hpp in the
// hived C++ tree.
package network

import (
	"crypto/sha256"
	"encoding/hex"
)

// Hash32 is a fixed-size 32-byte digest (sha256 output).
type Hash32 [32]byte

// Hex returns the lowercase hex representation of h.
func (h Hash32) Hex() string { return hex.EncodeToString(h[:]) }

// MustHexHash32 parses a 64-character hex string into a Hash32 or panics.
// Used only at package init for consensus-frozen constants.
func MustHexHash32(s string) Hash32 {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		panic("network: invalid hex hash: " + s)
	}
	var h Hash32
	copy(h[:], b)
	return h
}

// Common consensus timing constants — mainnet and testnet share these.
//
// Sources (config.hpp):
//
//	HIVE_BLOCK_INTERVAL          = 3 seconds
//	HIVE_BLOCKS_PER_HOUR         = 1200
//	HIVE_BLOCKS_PER_DAY          = 28800
//	HIVE_BLOCKS_PER_YEAR         = 10512000
//	HIVE_MAX_WITNESSES           = 21
const (
	BlockIntervalSec = 3
	BlocksPerHour    = 60 * 60 / BlockIntervalSec
	BlocksPerDay     = 24 * BlocksPerHour
	BlocksPerYear    = 365 * BlocksPerDay
	MaxWitnesses     = 21
)

// sha256OfString returns the sha256 digest of the given UTF-8 string.
// Used to derive the testnet chain ID.
func sha256OfString(s string) Hash32 {
	sum := sha256.Sum256([]byte(s))
	return Hash32(sum)
}

// BlockchainVersionNum is HIVE_BLOCKCHAIN_VERSION packed the way
// hive::protocol::version stores it: (major << 24) | (hardfork << 16) |
// revision, with revision occupying the low SIXTEEN bits
// (libraries/protocol/include/hive/protocol/version.hpp:30).
//
// This is the value a produced block carries in its version header extension,
// and applying that extension is what sets the producing witness's
// running_version.
const BlockchainVersionNum uint32 = uint32(BlockchainVersionMajor)<<24 |
	uint32(BlockchainVersionHardfork)<<16 |
	uint32(BlockchainVersionRevision)

// BlockchainVersion is the human-readable form database_api.get_version
// reports, e.g. "1.28.7".
var BlockchainVersion = FormatVersion(BlockchainVersionNum)

// FormatVersion renders a packed v_num as hived's version::operator string()
// does: major.hardfork.revision, with revision taken from the low 16 bits.
//
// Note the field widths — major and hardfork are one byte each, revision is
// two. Reading revision as a single byte silently renders 1.28.7 as "28.0.7".
func FormatVersion(v uint32) string {
	return itoa(v>>24&0xFF) + "." + itoa(v>>16&0xFF) + "." + itoa(v&0xFFFF)
}

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
