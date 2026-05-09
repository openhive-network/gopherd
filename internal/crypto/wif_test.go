package crypto

import (
	"crypto/sha256"
	"strings"
	"testing"
)

// TestWIFRoundTripFixedSeed checks Encode(Decode(x)) == x for an arbitrary
// deterministic key.
func TestWIFRoundTripFixedSeed(t *testing.T) {
	priv := fixedPrivKey(t)
	wif, err := EncodeWIF(priv)
	if err != nil {
		t.Fatalf("EncodeWIF: %v", err)
	}
	if !strings.HasPrefix(wif, "5") {
		t.Fatalf("WIF for an uncompressed mainnet key must start with 5, got %q", wif)
	}
	got, err := DecodeWIF(wif)
	if err != nil {
		t.Fatalf("DecodeWIF(%q): %v", wif, err)
	}
	reenc, err := EncodeWIF(got)
	if err != nil {
		t.Fatalf("re-EncodeWIF: %v", err)
	}
	if reenc != wif {
		t.Fatalf("WIF round-trip mismatch:\n got %q\nwant %q", reenc, wif)
	}
}

// TestWIFInitminerVector validates against the documented testnet
// "init_key" => "5JNHfZYKGaomSFvd4NUdQ9qMcEAC43kujbfjueTHpVapX1Kzq2n" mapping
// from tests/python/test_tools_tests/unit_tests/key_generation_tests/test_key_generation.py
// in the hived tree, plus the expected mainnet-prefixed public key.
func TestWIFInitminerVector(t *testing.T) {
	const expectedWIF = "5JNHfZYKGaomSFvd4NUdQ9qMcEAC43kujbfjueTHpVapX1Kzq2n"
	const expectedPub = "STM6LLegbAgLAy28EHrffBVuANFWcFgmqRMW13wBmTExqFE9SCkg4"

	// regenerate(sha256("init_key")) is hived's testnet initminer derivation.
	seed := sha256.Sum256([]byte("init_key"))
	priv, err := PrivateKeyFromBytes(seed[:])
	if err != nil {
		t.Fatalf("PrivateKeyFromBytes: %v", err)
	}
	wif, err := EncodeWIF(priv)
	if err != nil {
		t.Fatalf("EncodeWIF: %v", err)
	}
	if wif != expectedWIF {
		t.Fatalf("EncodeWIF init_key:\n got %q\nwant %q", wif, expectedWIF)
	}
	if got := priv.PublicKey().EncodeBase58(); got != expectedPub {
		t.Fatalf("PublicKey().EncodeBase58() init_key:\n got %q\nwant %q", got, expectedPub)
	}

	dec, err := DecodeWIF(expectedWIF)
	if err != nil {
		t.Fatalf("DecodeWIF: %v", err)
	}
	if got := dec.PublicKey().EncodeBase58(); got != expectedPub {
		t.Fatalf("DecodeWIF then PublicKey:\n got %q\nwant %q", got, expectedPub)
	}
}

func TestWIFCorruptChecksum(t *testing.T) {
	priv := fixedPrivKey(t)
	wif, err := EncodeWIF(priv)
	if err != nil {
		t.Fatal(err)
	}
	// Flip one character in the body.
	bad := []byte(wif)
	for i := 1; i < len(bad); i++ {
		if bad[i] != '2' {
			bad[i] = '2'
			break
		}
	}
	if _, err := DecodeWIF(string(bad)); err == nil {
		t.Fatal("expected checksum error on corrupted WIF")
	}
}

func TestWIFBadPrefix(t *testing.T) {
	// Build a 0x81-prefixed WIF (wrong version byte).
	payload := make([]byte, 1+32)
	payload[0] = 0x81
	for i := range payload[1:] {
		payload[1+i] = byte(i)
	}
	check1 := sha256.Sum256(payload)
	check2 := sha256.Sum256(check1[:])
	out := append(payload, check2[:4]...)
	bad := Base58Encode(out)
	if _, err := DecodeWIF(bad); err == nil {
		t.Fatal("expected version-prefix error")
	}
}

func TestWIFLegacySingleSHA256(t *testing.T) {
	// Hived's `generate_from_base58` accepts both single- and double-SHA256
	// checksums. Synthesize a single-SHA256 form and confirm Decode works.
	priv := fixedPrivKey(t)
	secret := priv.Bytes()
	payload := append([]byte{WIFPrefix}, secret...)
	check1 := sha256.Sum256(payload)
	out := append(payload, check1[:4]...)
	wif := Base58Encode(out)
	dec, err := DecodeWIF(wif)
	if err != nil {
		t.Fatalf("legacy single-SHA256 WIF should decode: %v", err)
	}
	if dec.PublicKey() != priv.PublicKey() {
		t.Fatal("legacy WIF decode produced a different key")
	}
}
