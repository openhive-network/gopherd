package crypto

import (
	"crypto/rand"
	"crypto/sha512"
	"errors"

	dcrec "github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// SharedSecret512 is the 64-byte output of ECDH-then-SHA-512, matching
// fc::ecc::private_key::get_shared_secret in libfc.
//
// hived's stcp_socket uses this as the AES key/IV seed.
type SharedSecret512 [64]byte

// GenerateEphemeralKey returns a fresh secp256k1 keypair suitable for the
// stcp ECDHE handshake.
func GenerateEphemeralKey() (*PrivateKey, error) {
	k, err := dcrec.GeneratePrivateKeyFromRand(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &PrivateKey{k: k}, nil
}

// SharedSecret performs ECDH between priv (local ephemeral) and remotePub
// (remote ephemeral) and returns SHA-512 of the resulting X-coordinate.
//
// This mirrors `fc::ecc::private_key::get_shared_secret`:
//
//   pub = pubkey_tweak_mul(remote_pub, priv)        // P = priv*Q
//   serialized = secp256k1_ec_pubkey_serialize(pub) // 33 bytes [parity || X]
//   sha512(serialized + 1, serialized.size() - 1)   // sha512(X), 32 bytes input
//
// dcrec's GenerateSharedSecret returns the X coordinate as a big-endian
// 32-byte value, which is the same byte layout as the trailing 32 bytes of
// the compressed serialization in libsecp256k1 — so we can hash it directly.
func SharedSecret(priv *PrivateKey, remotePub PublicKey) (SharedSecret512, error) {
	var out SharedSecret512
	if priv == nil || priv.k == nil {
		return out, errors.New("crypto: nil private key")
	}
	pub, err := dcrec.ParsePubKey(remotePub[:])
	if err != nil {
		return out, err
	}
	x := dcrec.GenerateSharedSecret(priv.k, pub)
	if len(x) != 32 {
		return out, errors.New("crypto: shared secret X must be 32 bytes")
	}
	sum := sha512.Sum512(x)
	copy(out[:], sum[:])
	return out, nil
}
