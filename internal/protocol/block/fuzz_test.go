package block

import (
	"bytes"
	"testing"

	"gopherd/internal/testutil/corpus"
)

// FuzzUnmarshalSignedHeader decodes a signed block header from arbitrary
// bytes, as the node does for every block a peer sends before anything else
// is checked. It must never panic, and the decoded header must be stable
// under re-encoding. It need not reproduce the input: like fc, the decoder
// accepts non-minimal varints (a witness-name length of 80 00), and neither
// node depends on re-encoding, since hived and gopherd both take the block id
// and signing digest from the bytes as received (full_block.cpp:145,156).
// Mainnet headers must round-trip exactly (TestSignedHeaderSeedsRoundTrip).
//
// Seeds under testdata/fuzz/FuzzUnmarshalSignedHeader are mainnet headers
// from every million blocks plus ones carrying extensions (internal/fuzzseed);
// keep that directory mainnet-only, since every entry must round-trip exactly.
func FuzzUnmarshalSignedHeader(f *testing.F) {
	f.Add(make([]byte, 20+4+1+20+1+65))
	// A non-minimal varint for the witness-name length (80 00 = 0), which fc
	// accepts too: decodes, re-encodes as 00.
	nonMinimal := append(make([]byte, 24), 0x80, 0x00)
	f.Add(append(nonMinimal, make([]byte, 20+1+65)...))
	f.Fuzz(func(t *testing.T, src []byte) {
		sh, n, err := UnmarshalSignedHeader(src)
		if err != nil {
			return
		}
		if n <= 0 || n > len(src) {
			t.Fatalf("consumed %d of %d bytes", n, len(src))
		}
		enc := MarshalSignedHeader(nil, &sh)
		sh2, n2, err := UnmarshalSignedHeader(enc)
		if err != nil || n2 != len(enc) {
			t.Fatalf("re-encoding does not decode cleanly (err=%v, %d of %d)\n in: %x\nenc: %x", err, n2, len(enc), src[:n], enc)
		}
		if enc2 := MarshalSignedHeader(nil, &sh2); !bytes.Equal(enc2, enc) {
			t.Fatalf("encoding is not stable\nenc:  %x\nenc2: %x", enc, enc2)
		}
		if _, err := ParseHeaderExtensions(sh.Extensions); err != nil {
			// Accepted as a header but not as extensions is allowed (the
			// parse is lazy), as long as neither panics.
			return
		}
	})
}

// FuzzParseHeaderExtensions pins that header extensions, which carry the
// hardfork version votes and HF29's state stamp, decode without panicking and
// that the decoded set is stable under re-encoding. A flat_set decodes by
// insertion (sorted, first of a tag wins), so unsorted or repeated input may
// legitimately re-encode shorter or reordered.
func FuzzParseHeaderExtensions(f *testing.F) {
	f.Add([]byte{0})
	f.Add([]byte{1, 1, 0x1c, 0x09, 0x01, 0x00})
	f.Add([]byte{2, 3, 0xaa, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0x1c, 0x09, 0x01, 0x00})
	f.Fuzz(func(t *testing.T, raw []byte) {
		exts, err := ParseHeaderExtensions(raw)
		if err != nil || len(raw) == 0 {
			return
		}
		enc := AppendHeaderExtensions(nil, exts)
		exts2, err := ParseHeaderExtensions(enc)
		if err != nil {
			t.Fatalf("re-encoding does not parse: %v\n in: %x\nenc: %x", err, raw, enc)
		}
		if enc2 := AppendHeaderExtensions(nil, exts2); !bytes.Equal(enc2, enc) {
			t.Fatalf("encoding is not stable\nenc:  %x\nenc2: %x", enc, enc2)
		}
	})
}

// TestSignedHeaderSeedsRoundTrip pins that every mainnet header in the seed
// corpus re-encodes to exactly its bytes.
func TestSignedHeaderSeedsRoundTrip(t *testing.T) {
	seeds := corpus.Load(t, "FuzzUnmarshalSignedHeader")
	if len(seeds) == 0 {
		t.Skip("no seed corpus")
	}
	for _, src := range seeds {
		sh, n, err := UnmarshalSignedHeader(src)
		if err != nil || n != len(src) {
			t.Fatalf("seed %x: err=%v, consumed %d of %d", src, err, n, len(src))
		}
		if enc := MarshalSignedHeader(nil, &sh); !bytes.Equal(enc, src) {
			t.Fatalf("seed re-encodes differently\n in: %x\nout: %x", src, enc)
		}
	}
}
