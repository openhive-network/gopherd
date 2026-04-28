package crypto

import (
	"bytes"
	"errors"
	"strings"

	"hived-go/internal/protocol/network"
)

// PublicKeySize is the on-wire size of a compressed secp256k1 public key.
const PublicKeySize = 33

// PublicKey is a compressed secp256k1 public key. The zero value is invalid;
// callers should construct via PublicKeyFromBytes or DecodePublicKey.
type PublicKey [PublicKeySize]byte

// IsZero reports whether p is the all-zero (invalid) key.
func (p PublicKey) IsZero() bool {
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
}

// PublicKeyFromBytes copies a 33-byte compressed key, returning an error if
// the input length is wrong. We do NOT validate that the bytes form a point
// on the secp256k1 curve here — that's done in internal/crypto/sigverify when
// the key is actually used.
func PublicKeyFromBytes(b []byte) (PublicKey, error) {
	var p PublicKey
	if len(b) != PublicKeySize {
		return p, errors.New("crypto: public key must be 33 bytes")
	}
	copy(p[:], b)
	return p, nil
}

// Bytes returns a copy of the underlying 33 bytes.
func (p PublicKey) Bytes() []byte { return append([]byte(nil), p[:]...) }

// EncodeBase58 encodes the key with the network's address prefix, matching
// `public_key::to_base58_with_prefix`. The checksum is the first 4 bytes of
// RIPEMD-160(key).
//
// Wire form: <prefix><base58(33-byte-key || 4-byte-checksum)>.
func (p PublicKey) EncodeBase58() string {
	body := encodeKeyWithChecksum(p[:])
	return network.AddressPrefix + Base58Encode(body)
}

// String returns EncodeBase58 — useful for debugging / log output.
func (p PublicKey) String() string { return p.EncodeBase58() }

// DecodePublicKey parses the network-prefixed base58 form (e.g. "STM…") into
// a PublicKey, validating the 4-byte RIPEMD-160 checksum.
func DecodePublicKey(s string) (PublicKey, error) {
	prefix := network.AddressPrefix
	var p PublicKey
	if !strings.HasPrefix(s, prefix) {
		return p, errors.New("crypto: missing " + prefix + " prefix")
	}
	body, err := Base58Decode(s[len(prefix):])
	if err != nil {
		return p, err
	}
	if len(body) != PublicKeySize+4 {
		return p, errors.New("crypto: decoded key must be 37 bytes (33 key + 4 checksum)")
	}
	want := RIPEMD160(body[:PublicKeySize])
	if !bytes.Equal(body[PublicKeySize:], want[:4]) {
		return p, errors.New("crypto: public key checksum mismatch")
	}
	copy(p[:], body[:PublicKeySize])
	return p, nil
}

// encodeKeyWithChecksum returns key||first4(ripemd160(key)).
func encodeKeyWithChecksum(key []byte) []byte {
	cs := RIPEMD160(key)
	out := make([]byte, len(key)+4)
	copy(out, key)
	copy(out[len(key):], cs[:4])
	return out
}
