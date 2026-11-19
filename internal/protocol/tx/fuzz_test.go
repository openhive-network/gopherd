package tx

import (
	"bytes"
	"testing"

	"gopherd/internal/fcraw"
	"gopherd/internal/testutil/corpus"
)

// FuzzReadSignedTransaction decodes a signed transaction from arbitrary
// bytes, as the node does for every transaction a peer relays or a client
// broadcasts. Decoding must never panic, the byte ranges it reports must lie
// inside the input, and the decoded transaction must be stable under
// re-encoding (see op.FuzzReadOp). The signature-skipping reader used on the
// replay path must agree with it on what it accepts and where it ends.
//
// Seeds under testdata/fuzz/FuzzReadSignedTransaction are mainnet
// transactions, one per leading operation type (internal/fuzzseed).
func FuzzReadSignedTransaction(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, src []byte) {
		r := fcraw.NewReader(src)
		res, err := ReadSignedTransaction(r)
		skip, skipErr := ReadSignedTransactionSkipSigs(fcraw.NewReader(src))
		if (err == nil) != (skipErr == nil) {
			t.Fatalf("ReadSignedTransaction err=%v, SkipSigs err=%v", err, skipErr)
		}
		if err != nil {
			return
		}
		if !(0 <= res.UnsignedStart && res.UnsignedStart <= res.UnsignedEnd && res.UnsignedEnd <= res.SignedEnd && res.SignedEnd <= len(src)) {
			t.Fatalf("ranges [%d,%d) [%d,%d) outside a %d-byte input", res.UnsignedStart, res.UnsignedEnd, res.UnsignedStart, res.SignedEnd, len(src))
		}
		if skip.UnsignedEnd != res.UnsignedEnd || skip.SignedEnd != res.SignedEnd {
			t.Fatalf("SkipSigs ranges (%d,%d) differ from (%d,%d)", skip.UnsignedEnd, skip.SignedEnd, res.UnsignedEnd, res.SignedEnd)
		}
		st := res.SignedTx
		enc := AppendSignedTransaction(nil, &st)
		r2 := fcraw.NewReader(enc)
		res2, err := ReadSignedTransaction(r2)
		if err != nil || r2.Remaining() != 0 {
			t.Fatalf("re-encoding does not decode cleanly (err=%v, %d left)\nenc: %x", err, r2.Remaining(), enc)
		}
		if enc2 := AppendSignedTransaction(nil, &res2.SignedTx); !bytes.Equal(enc2, enc) {
			t.Fatalf("encoding is not stable\nenc:  %x\nenc2: %x", enc, enc2)
		}
	})
}

// FuzzReadBlockTxCount pins that a declared transaction count is never larger
// than the remaining bytes could hold (each transaction is at least 11 bytes).
// The count is peer-controlled, and callers size slices from it.
func FuzzReadBlockTxCount(f *testing.F) {
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0x0f})
	f.Add([]byte{0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, src []byte) {
		r := fcraw.NewReader(src)
		n, err := ReadBlockTxCount(r)
		if err != nil {
			return
		}
		if uint64(n)*11 > uint64(r.Remaining()) {
			t.Fatalf("count %d accepted with %d bytes left", n, r.Remaining())
		}
	})
}

// TestReadSignedTransactionSeedsRoundTrip pins that every mainnet transaction
// in the seed corpus re-encodes to exactly its bytes, signed and unsigned.
func TestReadSignedTransactionSeedsRoundTrip(t *testing.T) {
	seeds := corpus.Load(t, "FuzzReadSignedTransaction")
	if len(seeds) == 0 {
		t.Skip("no seed corpus")
	}
	for _, src := range seeds {
		r := fcraw.NewReader(src)
		res, err := ReadSignedTransaction(r)
		if err != nil || r.Remaining() != 0 {
			t.Fatalf("seed %x: err=%v, %d trailing", src, err, r.Remaining())
		}
		st := res.SignedTx
		if enc := AppendSignedTransaction(nil, &st); !bytes.Equal(enc, src) {
			t.Fatalf("seed re-encodes differently\n in: %x\nout: %x", src, enc)
		}
		if enc := AppendTransactionBody(nil, &st.Transaction); !bytes.Equal(enc, src[res.UnsignedStart:res.UnsignedEnd]) {
			t.Fatalf("seed's unsigned range re-encodes differently")
		}
	}
}
