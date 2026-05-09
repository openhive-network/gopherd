package crypto

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// Round-trip: both sides of ECDH must derive the same 64-byte shared
// secret. Doesn't validate byte-for-byte equality with hived (that's
// covered by interop testing in the smoke-test layer); only that the
// math works.
func TestECDHRoundTrip(t *testing.T) {
	priv1, err := GenerateEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	priv2, err := GenerateEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	pub1 := priv1.PublicKey()
	pub2 := priv2.PublicKey()

	s12, err := SharedSecret(priv1, pub2)
	if err != nil {
		t.Fatal(err)
	}
	s21, err := SharedSecret(priv2, pub1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s12[:], s21[:]) {
		t.Fatalf("shared secret mismatch: %x vs %x", s12, s21)
	}
}

// TestSharedSecretMatchesHived pins our ECDH+SHA512 against a vector
// generated from libfc with scalars 0x01||0...0 and 0x02||0...0:
//
//   privA = 0100000000000000000000000000000000000000000000000000000000000000
//   privB = 0200000000000000000000000000000000000000000000000000000000000000
//   pubA  = 028c28a97bf8298bc0d23d8c749452a32e694b65e30a9472a3954ab30fe5324caa
//   pubB  = 03ab1ac1872a38a2f196bed5a6047f0da2c8130fe8de49fc4d5dfb201f7611d8e2
//   shared = sha512(X(privA*pubB))
//          = 481394b3952799c1094901d9b3fbc99d95161bacd6ccc491e29b077cd4ae3e4b
//            828d4d6f37e1931002ebad22f02b9a74839e880ca11bf81bcac8f49f9e0bf459
//
// (Note: fc::ecc::private_key::regenerate(fc::sha256(buf, 32)) does NOT hash
// `buf` — fc::sha256 is a 32-byte container with that constructor, so the
// scalar is exactly the buffer bytes.)
func TestSharedSecretMatchesHived(t *testing.T) {
	scalarA := make([]byte, 32)
	scalarA[0] = 0x01
	scalarB := make([]byte, 32)
	scalarB[0] = 0x02
	wantSecret := "481394b3952799c1094901d9b3fbc99d95161bacd6ccc491e29b077cd4ae3e4b" +
		"828d4d6f37e1931002ebad22f02b9a74839e880ca11bf81bcac8f49f9e0bf459"

	privA, err := PrivateKeyFromBytes(scalarA)
	if err != nil {
		t.Fatal(err)
	}
	privB, err := PrivateKeyFromBytes(scalarB)
	if err != nil {
		t.Fatal(err)
	}
	pubA := privA.PublicKey()
	pubB := privB.PublicKey()
	if got := hex.EncodeToString(pubA[:]); got != "028c28a97bf8298bc0d23d8c749452a32e694b65e30a9472a3954ab30fe5324caa" {
		t.Fatalf("pubA derivation differs from libfc: %s", got)
	}
	if got := hex.EncodeToString(pubB[:]); got != "03ab1ac1872a38a2f196bed5a6047f0da2c8130fe8de49fc4d5dfb201f7611d8e2" {
		t.Fatalf("pubB derivation differs from libfc: %s", got)
	}
	sAB, err := SharedSecret(privA, pubB)
	if err != nil {
		t.Fatal(err)
	}
	sBA, err := SharedSecret(privB, pubA)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(sAB[:]); got != wantSecret {
		t.Errorf("AB secret: got %s want %s", got, wantSecret)
	}
	if got := hex.EncodeToString(sBA[:]); got != wantSecret {
		t.Errorf("BA secret: got %s want %s", got, wantSecret)
	}
}
