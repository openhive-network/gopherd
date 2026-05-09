// Package genesis — wif_helper.go provides a minimal WIF (Wallet Import
// Format) encoder/decoder used by the testnet bootstrap. The dedicated
// `internal/crypto/wif.go` lives on the `agent-sign` branch; until it
// merges this self-contained helper lets the testnet bootstrap and tests
// run without a cross-branch dependency.
//
// WIF is base58check(prefix || 32-byte secret), where prefix is 0x80
// (Bitcoin mainnet legacy). Hive uses the SAME prefix on both mainnet
// and testnet — fc::ecc::private_key::serialize_to_wif hard-codes 0x80.
// The 4-byte checksum is the first 4 bytes of sha256(sha256(payload)).
package genesis

import (
	"errors"

	"hived-go/internal/crypto"
)

// wifPrefix is the leading byte for Hive WIF strings (matches Bitcoin
// mainnet legacy and fc::ecc::private_key::wif_prefix).
const wifPrefix byte = 0x80

// EncodeWIF encodes a 32-byte secp256k1 secret into the WIF string form
// "5xxxxxxxx...". The leading character is always '5' for the
// uncompressed-style WIF (single-byte prefix, no compression flag) that
// hived wallets emit and accept.
func EncodeWIF(secret []byte) (string, error) {
	if len(secret) != 32 {
		return "", errors.New("genesis: WIF secret must be 32 bytes")
	}
	payload := make([]byte, 33)
	payload[0] = wifPrefix
	copy(payload[1:], secret)
	cs := doubleSHA256(payload)
	out := make([]byte, 0, 37)
	out = append(out, payload...)
	out = append(out, cs[:4]...)
	return crypto.Base58Encode(out), nil
}

// DecodeWIF reverses EncodeWIF. Returns ErrInvalidWIF on any structural
// or checksum failure.
func DecodeWIF(s string) ([]byte, error) {
	body, err := crypto.Base58Decode(s)
	if err != nil {
		return nil, err
	}
	if len(body) != 37 || body[0] != wifPrefix {
		return nil, errors.New("genesis: WIF must be 37 bytes with 0x80 prefix")
	}
	cs := doubleSHA256(body[:33])
	if !equalBytes(cs[:4], body[33:]) {
		return nil, errors.New("genesis: WIF checksum mismatch")
	}
	out := make([]byte, 32)
	copy(out, body[1:33])
	return out, nil
}

// doubleSHA256 returns the 32-byte sha256(sha256(data)) digest used for
// WIF / base58check checksums.
func doubleSHA256(data []byte) [32]byte {
	first := crypto.SHA256(data)
	return crypto.SHA256(first[:])
}

// equalBytes is a constant-time-ish equality helper. Used only for
// checksum comparison; not adversarial input here so plain compare is
// fine.
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
