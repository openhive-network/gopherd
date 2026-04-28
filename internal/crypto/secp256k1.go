package crypto

import (
	"crypto/sha256"
	"errors"

	dcrec "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// CompactSignature is the Hive on-wire signature format: 1 byte recovery id
// (with offsets baked in by the C++ code) followed by 32-byte r and 32-byte s.
//
// fc::ecc::compact_signature is `fc::array<unsigned char, 65>`. The first
// byte encodes the recovery id with high bits 0x1B + (compressed?4:0).
// libsecp256k1's `ecdsa_sign_compact` produces this layout natively.
type CompactSignature [65]byte

// Sigs that pass through Hive consensus must be canonical (BIP-0062 low-s).
// The C++ implementation re-rolls the nonce until the resulting signature is
// canonical. We do the same here, capping the retry count at a generous
// bound to surface bugs rather than spin forever.
const maxCanonicalSignAttempts = 25

// SignCompact signs digest with priv, returning a 65-byte recoverable
// compact signature. The signature is guaranteed to be BIP-0062 canonical
// (low-s); priv must be a valid secp256k1 scalar.
//
// digest is a 32-byte hash (typically SHA-256 of chain_id || serialized_tx).
func SignCompact(priv *PrivateKey, digest [32]byte) (CompactSignature, error) {
	var sig CompactSignature
	if priv == nil || priv.k == nil {
		return sig, errors.New("crypto: nil private key")
	}
	for nonce := uint32(0); nonce < maxCanonicalSignAttempts; nonce++ {
		s := ecdsa.SignCompact(priv.k, digestForSign(digest, nonce), true)
		// The dcrec compact format is [recovery_id+27 (or +31 if compressed)] || r || s.
		// IsCanonical checks BIP-0062 low-s (matches fc::is_bip_0062_canonical).
		if isLowS(s[1:33], s[33:65]) {
			copy(sig[:], s)
			return sig, nil
		}
	}
	return sig, errors.New("crypto: failed to produce canonical signature after retries")
}

// digestForSign mixes a per-attempt nonce into the hash so that successive
// retries of SignCompact use different randomness. This matches the spirit
// of the C++ retry loop, which re-derives k via RFC6979 with an extra byte.
func digestForSign(digest [32]byte, nonce uint32) []byte {
	if nonce == 0 {
		return digest[:]
	}
	h := sha256.New()
	h.Write(digest[:])
	var n [4]byte
	n[0] = byte(nonce)
	n[1] = byte(nonce >> 8)
	n[2] = byte(nonce >> 16)
	n[3] = byte(nonce >> 24)
	h.Write(n[:])
	out := h.Sum(nil)
	return out
}

// RecoverPublicKey recovers the public key that produced sig over digest.
// Returns the compressed 33-byte key.
func RecoverPublicKey(digest [32]byte, sig CompactSignature) (PublicKey, error) {
	var p PublicKey
	pub, _, err := ecdsa.RecoverCompact(sig[:], digest[:])
	if err != nil {
		return p, err
	}
	compressed := pub.SerializeCompressed()
	if len(compressed) != PublicKeySize {
		return p, errors.New("crypto: recovered key is not 33 bytes")
	}
	copy(p[:], compressed)
	return p, nil
}

// VerifyCompact recovers the public key from sig and checks it equals expect.
// Returns nil if the signature is valid for digest under expect.
func VerifyCompact(expect PublicKey, digest [32]byte, sig CompactSignature) error {
	got, err := RecoverPublicKey(digest, sig)
	if err != nil {
		return err
	}
	if got != expect {
		return errors.New("crypto: signature does not match expected public key")
	}
	return nil
}

// IsCanonical reports whether sig is a low-s, BIP-0062 canonical signature.
// Hive's consensus rejects high-s signatures.
func (s CompactSignature) IsCanonical() bool {
	return isLowS(s[1:33], s[33:65])
}

// PrivateKey wraps a dcrec secp256k1 private key. The zero value is invalid.
type PrivateKey struct {
	k *dcrec.PrivateKey
}

// PrivateKeyFromBytes constructs a PrivateKey from a 32-byte big-endian scalar.
func PrivateKeyFromBytes(b []byte) (*PrivateKey, error) {
	if len(b) != 32 {
		return nil, errors.New("crypto: private key must be 32 bytes")
	}
	k := dcrec.PrivKeyFromBytes(b)
	return &PrivateKey{k: k}, nil
}

// PublicKey returns the compressed public key for priv.
func (p *PrivateKey) PublicKey() PublicKey {
	var out PublicKey
	pub := p.k.PubKey().SerializeCompressed()
	copy(out[:], pub)
	return out
}

// Bytes returns the 32-byte big-endian scalar. The returned slice should be
// treated as sensitive material.
func (p *PrivateKey) Bytes() []byte {
	b := p.k.Serialize()
	return b
}

// curveHalfOrder is n/2 for secp256k1, used for low-s canonical-sig check.
// secp256k1 n = FFFFFFFF FFFFFFFF FFFFFFFF FFFFFFFE BAAEDCE6 AF48A03B BFD25E8C D0364141.
// n/2     = 7FFFFFFF FFFFFFFF FFFFFFFF FFFFFFFF 5D576E73 57A4501D DFE92F46 681B20A0.
var curveHalfOrder = [32]byte{
	0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0x5d, 0x57, 0x6e, 0x73, 0x57, 0xa4, 0x50, 0x1d,
	0xdf, 0xe9, 0x2f, 0x46, 0x68, 0x1b, 0x20, 0xa0,
}

// isLowS reports whether the s component is in [1, n/2]. r is unused but
// taken to match the (r, s) signature shape.
func isLowS(_ []byte, s []byte) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < 32; i++ {
		if s[i] < curveHalfOrder[i] {
			return true
		}
		if s[i] > curveHalfOrder[i] {
			return false
		}
	}
	return true // equal — boundary is inclusive
}
