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
