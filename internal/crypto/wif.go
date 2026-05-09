package crypto

import (
	"bytes"
	"crypto/sha256"
	"errors"
)

// WIFPrefix is the leading version byte for a Bitcoin/Hive WIF private key
// (mainnet). Mirrors fc::ecc::private_key::to_base58 in
// libraries/fc/src/crypto/elliptic_common.cpp.
const WIFPrefix byte = 0x80

// EncodeWIF encodes priv as a Wallet Import Format string. The on-wire form
// is base58( 0x80 || 32-byte secret || checksum ) where checksum is the
// first 4 bytes of SHA256(SHA256(0x80 || secret)).
//
// Hived's encoder (`private_key::to_base58`) uses the double-SHA256 form.
// DecodeWIF accepts both single- and double-SHA256 checksums to match
// hived's `generate_from_base58` legacy fallback.
func EncodeWIF(priv *PrivateKey) (string, error) {
	if priv == nil {
		return "", errors.New("crypto: nil private key")
	}
	secret := priv.Bytes()
	if len(secret) != 32 {
		return "", errors.New("crypto: private key must be 32 bytes")
	}
	payload := make([]byte, 1+32)
	payload[0] = WIFPrefix
	copy(payload[1:], secret)

	check1 := sha256.Sum256(payload)
	check2 := sha256.Sum256(check1[:])

	out := make([]byte, 0, len(payload)+4)
	out = append(out, payload...)
	out = append(out, check2[:4]...)
	return Base58Encode(out), nil
}

// DecodeWIF reverses EncodeWIF. Validates the 4-byte checksum, accepting
// either single- or double-SHA256 form (mirrors `generate_from_base58`).
// Returns the parsed PrivateKey on success.
func DecodeWIF(wif string) (*PrivateKey, error) {
	body, err := Base58Decode(wif)
	if err != nil {
		return nil, err
	}
	if len(body) < 5 {
		return nil, errors.New("crypto: WIF too short")
	}
	if body[0] != WIFPrefix {
		return nil, errors.New("crypto: WIF missing 0x80 prefix")
	}
	payload := body[:len(body)-4]
	checksum := body[len(body)-4:]

	check1 := sha256.Sum256(payload)
	check2 := sha256.Sum256(check1[:])
	if !bytes.Equal(checksum, check2[:4]) && !bytes.Equal(checksum, check1[:4]) {
		return nil, errors.New("crypto: WIF checksum mismatch")
	}

	// Hived's WIF supports only the uncompressed form (33-byte payload).
	// Reject the 0x01 compressed-suffix form (34-byte payload) so we
	// don't silently misinterpret a Bitcoin-format WIF as Hive-format.
	secret := payload[1:]
	if len(secret) != 32 {
		return nil, errors.New("crypto: WIF body must be 32 bytes (uncompressed)")
	}
	return PrivateKeyFromBytes(secret)
}
