package crypto

import (
	"bytes"
	"testing"
)

func TestBase58RoundTrip(t *testing.T) {
	cases := [][]byte{
		{},
		{0x00},
		{0x00, 0x00},
		{0x01},
		{0x00, 0x01, 0x02, 0x03},
		{0xff, 0xee, 0xdd, 0xcc},
		[]byte("hello world"),
	}
	for _, c := range cases {
		enc := Base58Encode(c)
		dec, err := Base58Decode(enc)
		if err != nil {
			t.Fatalf("decode %q: %v", enc, err)
		}
		if !bytes.Equal(dec, c) {
			t.Fatalf("round-trip %x: encoded %q decoded %x", c, enc, dec)
		}
	}
}

func TestBase58LeadingZeros(t *testing.T) {
	// Each leading zero byte must produce one leading '1' character.
	enc := Base58Encode([]byte{0x00, 0x00, 0x00, 0x01})
	if enc[:3] != "111" {
		t.Fatalf("leading zeros lost: got %q", enc)
	}
}

func TestBase58RejectsInvalidChars(t *testing.T) {
	if _, err := Base58Decode("STM0"); err == nil {
		t.Fatal("expected error on '0' (not in alphabet)")
	}
	if _, err := Base58Decode("STMO"); err == nil {
		t.Fatal("expected error on 'O' (not in alphabet)")
	}
	if _, err := Base58Decode("STMI"); err == nil {
		t.Fatal("expected error on 'I' (not in alphabet)")
	}
	if _, err := Base58Decode("STMl"); err == nil {
		t.Fatal("expected error on 'l' (not in alphabet)")
	}
}

// Bitcoin/Hive base58 vector: the all-zero 4-byte input encodes to "1111".
func TestBase58AllZeros(t *testing.T) {
	if got := Base58Encode([]byte{0, 0, 0, 0}); got != "1111" {
		t.Fatalf("Base58(0000) = %q, want \"1111\"", got)
	}
}
