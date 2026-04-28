package crypto

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

// fixedPrivKey returns a deterministic non-zero private key for tests.
func fixedPrivKey(t *testing.T) *PrivateKey {
	t.Helper()
	seed := sha256.Sum256([]byte("ghive-test-seed"))
	priv, err := PrivateKeyFromBytes(seed[:])
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func TestSignVerifyRoundTrip(t *testing.T) {
	priv := fixedPrivKey(t)
	pub := priv.PublicKey()

	digest := SHA256([]byte("hello world"))
	sig, err := SignCompact(priv, digest)
	if err != nil {
		t.Fatalf("SignCompact: %v", err)
	}
	if !sig.IsCanonical() {
		t.Fatal("signature must be canonical (low-s)")
	}

	got, err := RecoverPublicKey(digest, sig)
	if err != nil {
		t.Fatalf("RecoverPublicKey: %v", err)
	}
	if got != pub {
		t.Fatalf("recovered key mismatch: got %x, want %x", got, pub)
	}

	if err := VerifyCompact(pub, digest, sig); err != nil {
		t.Fatalf("VerifyCompact: %v", err)
	}

	// Tampered digest must fail.
	tampered := digest
	tampered[0] ^= 1
	if err := VerifyCompact(pub, tampered, sig); err == nil {
		t.Fatal("VerifyCompact on tampered digest must fail")
	}
}

func TestPublicKeyBase58RoundTrip(t *testing.T) {
	priv := fixedPrivKey(t)
	pub := priv.PublicKey()

	encoded := pub.EncodeBase58()
	decoded, err := DecodePublicKey(encoded)
	if err != nil {
		t.Fatalf("DecodePublicKey(%q): %v", encoded, err)
	}
	if decoded != pub {
		t.Fatalf("round-trip mismatch: got %x, want %x", decoded, pub)
	}

	// Mainnet keys must start with "STM".
	if encoded[:3] != "STM" {
		t.Fatalf("encoded key should start with STM, got %q", encoded)
	}
}

func TestDecodePublicKeyChecksumMismatch(t *testing.T) {
	priv := fixedPrivKey(t)
	enc := priv.PublicKey().EncodeBase58()
	// Flip a character in the body (skip the "STM" prefix).
	bad := []byte(enc)
	if bad[5] == '2' {
		bad[5] = '3'
	} else {
		bad[5] = '2'
	}
	if _, err := DecodePublicKey(string(bad)); err == nil {
		t.Fatal("expected checksum-mismatch error on tampered key")
	}
}

func TestDecodePublicKeyMissingPrefix(t *testing.T) {
	if _, err := DecodePublicKey("HIV1abc"); err == nil {
		t.Fatal("expected error when prefix is wrong")
	}
}

func TestKnownTestVector_HiveInitMinerKey(t *testing.T) {
	// Mainnet HIVE_INIT_PUBLIC_KEY_STR from libraries/protocol/include/hive/protocol/config.hpp.
	const initKey = "STM8GC13uCZbP44HzMLV6zPZGwVQ8Nt4Kji8PapsPiNq1BK153XTX"
	pub, err := DecodePublicKey(initKey)
	if err != nil {
		t.Fatalf("decoding HIVE_INIT_PUBLIC_KEY: %v", err)
	}
	// Re-encoding must produce the exact same string.
	if got := pub.EncodeBase58(); got != initKey {
		t.Fatalf("re-encoded init key:\n got %q\nwant %q", got, initKey)
	}
}

func TestPrivateKeyBytes(t *testing.T) {
	priv := fixedPrivKey(t)
	b := priv.Bytes()
	if len(b) != 32 {
		t.Fatalf("private key bytes must be 32, got %d", len(b))
	}
	round, err := PrivateKeyFromBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(round.Bytes(), b) {
		t.Fatal("private key round-trip changed bytes")
	}
}
