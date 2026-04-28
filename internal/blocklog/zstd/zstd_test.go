package zstd

import (
	"testing"

	zstdpkg "github.com/klauspost/compress/zstd"
)

func TestEmbeddedDictsAreZstd(t *testing.T) {
	if len(rawDicts) == 0 {
		t.Fatal("no dictionaries embedded")
	}
	for n, raw := range rawDicts {
		if len(raw) < 4 {
			t.Fatalf("dict %d: too short (%d bytes)", n, len(raw))
		}
		// Standard zstd magic.
		if raw[0] != 0x28 || raw[1] != 0xb5 || raw[2] != 0x2f || raw[3] != 0xfd {
			t.Errorf("dict %d: bad magic %x %x %x %x", n, raw[0], raw[1], raw[2], raw[3])
		}
	}
}

func TestDecompressDictionaryRoundsTrip(t *testing.T) {
	for n := range rawDicts {
		dec, err := getDecompressedDict(n)
		if err != nil {
			t.Errorf("decompress dict %d: %v", n, err)
			continue
		}
		if len(dec) == 0 {
			t.Errorf("dict %d: decompressed to 0 bytes", n)
		}
	}
}

// TestDecompressorReframesNoDictIDFrames exercises the magicless +
// no-DictionaryID code path — the only on-disk format hived writes.
// We can't synthesize this with klauspost as an encoder (no magicless
// option), so we hand-craft the smallest valid frame that round-trips
// back through our reframe + decode pipeline using a custom no-dict
// payload.
func TestDecompressorReframesNoDictIDFrames(t *testing.T) {
	// Encode "hello world" with klauspost (no dict), strip the standard
	// magic and any embedded DictionaryID, then feed the magicless bytes
	// through Decompress. Without a dict, FHD bits 0–1 are already 00, so
	// just dropping the magic produces the on-disk format we expect.
	enc, err := zstdpkg.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	original := []byte("hello world")
	full := enc.EncodeAll(original, nil)
	if len(full) < 4 {
		t.Fatalf("encoded too short: %x", full)
	}
	magicless := full[4:]

	d := NewDecompressor()
	defer d.Close()
	// Use the no-dict path (the synthetic frame doesn't reference a dict).
	decoded, err := d.DecompressNoDict(magicless, nil)
	if err != nil {
		t.Fatalf("DecompressNoDict: %v", err)
	}
	if string(decoded) != string(original) {
		t.Fatalf("round-trip mismatch:\n got %q\nwant %q", decoded, original)
	}
}

func TestDecompressorMissingDictError(t *testing.T) {
	d := NewDecompressor()
	defer d.Close()
	if _, err := d.Decompress([]byte{0x28, 0xb5, 0x2f, 0xfd}, 200, nil); err == nil {
		t.Fatal("expected error for missing dict 200")
	}
}
