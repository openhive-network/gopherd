// Package crypto wraps the cryptographic primitives used by Hive: SHA-256,
// RIPEMD-160, secp256k1 (recoverable ECDSA), and base58 with the STM/TST
// prefix scheme. Pure Go, no cgo.
//
// Provenance: matches libraries/fc/src/crypto/* in the hived C++ tree.
package crypto

import (
	"crypto/sha256"

	"golang.org/x/crypto/ripemd160"
)

// SHA256 returns the SHA-256 digest of data as a 32-byte array.
func SHA256(data []byte) [32]byte { return sha256.Sum256(data) }

// SHA224 returns the SHA-224 digest of data as a 28-byte array. Used in
// block_id construction (libraries/chain/full_block.cpp:construct_block_id).
func SHA224(data []byte) [28]byte { return sha256.Sum224(data) }

// SHA256Of2 hashes the concatenation of a and b without allocating an
// intermediate slice. Useful for tx ID = sha256(chain_id || serialized_tx)
// derivations.
func SHA256Of2(a, b []byte) [32]byte {
	h := sha256.New()
	h.Write(a)
	h.Write(b)
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// RIPEMD160 returns the 20-byte RIPEMD-160 digest of data.
//
// golang.org/x/crypto/ripemd160 is marked deprecated upstream because RIPEMD-160
// is no longer recommended for new use, but Hive's consensus depends on it
// (block IDs, tx IDs, key checksums). We accept the deprecation.
func RIPEMD160(data []byte) [20]byte {
	h := ripemd160.New()
	h.Write(data)
	var out [20]byte
	h.Sum(out[:0])
	return out
}

// RIPEMD160OfSHA256 = RIPEMD-160 of SHA-256 of data. This is the "Hash160"
// pattern used for Bitcoin-style addresses; Hive's public_key::fingerprint()
// uses it.
func RIPEMD160OfSHA256(data []byte) [20]byte {
	s := sha256.Sum256(data)
	return RIPEMD160(s[:])
}
