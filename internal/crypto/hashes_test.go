package crypto

import (
	"encoding/hex"
	"testing"
)

func TestSHA256KnownVector(t *testing.T) {
	got := SHA256([]byte("abc"))
	want, _ := hex.DecodeString("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
	if got != [32]byte(want) {
		t.Fatalf("SHA256(abc) = %x, want %x", got, want)
	}
}

func TestSHA224KnownVector(t *testing.T) {
	got := SHA224([]byte("abc"))
	want, _ := hex.DecodeString("23097d223405d8228642a477bda255b32aadbce4bda0b3f7e36c9da7")
	if got != [28]byte(want) {
		t.Fatalf("SHA224(abc) = %x, want %x", got, want)
	}
}

func TestRIPEMD160KnownVector(t *testing.T) {
	// From the original RIPEMD-160 paper test vectors.
	got := RIPEMD160([]byte("abc"))
	want, _ := hex.DecodeString("8eb208f7e05d987a9b044a8e98c6b087f15a0bfc")
	if got != [20]byte(want) {
		t.Fatalf("RIPEMD160(abc) = %x, want %x", got, want)
	}
}

func TestSHA256Of2(t *testing.T) {
	a := []byte("hello ")
	b := []byte("world")
	got := SHA256Of2(a, b)
	want := SHA256(append(append([]byte{}, a...), b...))
	if got != want {
		t.Fatalf("SHA256Of2 mismatch: got %x, want %x", got, want)
	}
}
