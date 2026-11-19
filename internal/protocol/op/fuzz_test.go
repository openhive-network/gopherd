package op

import (
	"bytes"
	"testing"

	"gopherd/internal/fcraw"
	"gopherd/internal/testutil/corpus"
)

// FuzzReadOp decodes an operation from arbitrary bytes. Operations arrive
// inside transactions from any peer, so decoding must never panic. When it
// succeeds, the decoded value must be stable: its encoding decodes again to
// a value with the same encoding, consuming exactly those bytes. Canonical
// input (every mainnet seed) must re-encode to exactly the bytes consumed.
// Non-canonical input may legitimately re-encode differently: hived decodes
// flat_set / flat_map members by inserting, which sorts and drops repeats.
//
// Seeds under testdata/fuzz/FuzzReadOp are one mainnet operation per type
// (internal/fuzzseed regenerates them).
func FuzzReadOp(f *testing.F) {
	f.Add([]byte{0x00})
	f.Fuzz(func(t *testing.T, src []byte) {
		r := fcraw.NewReader(src)
		o, err := ReadOp(r)
		if err != nil {
			return
		}
		consumed := src[:r.Pos()]
		enc := AppendOp(nil, o)
		r2 := fcraw.NewReader(enc)
		o2, err := ReadOp(r2)
		if err != nil {
			t.Fatalf("%s: re-encoding does not decode: %v\nin:  %x\nenc: %x", o.Tag.Name(), err, consumed, enc)
		}
		if r2.Remaining() != 0 {
			t.Fatalf("%s: re-encoding has %d trailing bytes", o.Tag.Name(), r2.Remaining())
		}
		if enc2 := AppendOp(nil, o2); !bytes.Equal(enc2, enc) {
			t.Fatalf("%s: encoding is not stable\nin:   %x\nenc:  %x\nenc2: %x", o.Tag.Name(), consumed, enc, enc2)
		}

	})
}

// TestReadOpSeedsRoundTrip pins that every mainnet operation in the seed
// corpus decodes and re-encodes to exactly its bytes.
func TestReadOpSeedsRoundTrip(t *testing.T) {
	seeds := corpus.Load(t, "FuzzReadOp")
	if len(seeds) == 0 {
		t.Skip("no seed corpus")
	}
	for _, src := range seeds {
		r := fcraw.NewReader(src)
		o, err := ReadOp(r)
		if err != nil {
			t.Fatalf("seed %x: %v", src, err)
		}
		if r.Remaining() != 0 {
			t.Fatalf("%s seed: %d trailing bytes", o.Tag.Name(), r.Remaining())
		}
		if enc := AppendOp(nil, o); !bytes.Equal(enc, src) {
			t.Fatalf("%s seed re-encodes differently\n in: %x\nout: %x", o.Tag.Name(), src, enc)
		}
	}
}
