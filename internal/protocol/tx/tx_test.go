package tx

import (
	"bytes"
	"encoding/hex"
	"testing"

	"hived-go/internal/crypto"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
)

func TestEmptyTransactionRoundTrip(t *testing.T) {
	st := SignedTransaction{
		Transaction: Transaction{
			RefBlockNum:    100,
			RefBlockPrefix: 0xdeadbeef,
			Expiration:     1234567890,
		},
	}
	wire := AppendSignedTransaction(nil, &st)
	res, err := ReadSignedTransaction(fcraw.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	if res.SignedTx.RefBlockNum != 100 || res.SignedTx.RefBlockPrefix != 0xdeadbeef ||
		res.SignedTx.Expiration != 1234567890 {
		t.Fatalf("header roundtrip: %+v", res.SignedTx.Transaction)
	}
	if len(res.SignedTx.Operations) != 0 {
		t.Errorf("expected 0 ops, got %d", len(res.SignedTx.Operations))
	}
	if len(res.SignedTx.Signatures) != 0 {
		t.Errorf("expected 0 sigs, got %d", len(res.SignedTx.Signatures))
	}
	wire2 := AppendSignedTransaction(nil, &res.SignedTx)
	if !bytes.Equal(wire, wire2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", wire2, wire)
	}
}

func TestTransactionWithOpsAndSignatures(t *testing.T) {
	transferOp := op.Transfer{
		From:   "alice",
		To:     "bob",
		Amount: asset.Asset{Amount: 1_000, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}},
		Memo:   "hi",
	}
	voteOp := op.Vote{Voter: "alice", Author: "bob", Permlink: "post", Weight: 5000}

	var sig1, sig2 [SignatureSize]byte
	for i := range sig1 {
		sig1[i] = byte(i)
	}
	for i := range sig2 {
		sig2[i] = byte(255 - i)
	}

	st := SignedTransaction{
		Transaction: Transaction{
			RefBlockNum:    7,
			RefBlockPrefix: 0xbeefcafe,
			Expiration:     2_000_000,
			Operations:     op.Ops(transferOp, voteOp),
		},
		Signatures: [][SignatureSize]byte{sig1, sig2},
	}
	wire := AppendSignedTransaction(nil, &st)

	res, err := ReadSignedTransaction(fcraw.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SignedTx.Operations) != 2 {
		t.Fatalf("ops count = %d", len(res.SignedTx.Operations))
	}
	if res.SignedTx.Operations[0].OpTag() != op.TagTransfer {
		t.Errorf("first op should be transfer, got %s", res.SignedTx.Operations[0].OpTag().Name())
	}
	if res.SignedTx.Operations[1].OpTag() != op.TagVote {
		t.Errorf("second op should be vote, got %s", res.SignedTx.Operations[1].OpTag().Name())
	}
	if len(res.SignedTx.Signatures) != 2 {
		t.Fatalf("sig count = %d", len(res.SignedTx.Signatures))
	}
	if res.SignedTx.Signatures[0] != sig1 || res.SignedTx.Signatures[1] != sig2 {
		t.Errorf("signature byte mismatch")
	}

	// Byte-range sanity: digest is sha256 of the unsigned range, merkle digest of the full range.
	dig := DigestFromBytes(wire[res.UnsignedStart:res.UnsignedEnd])
	wantUnsignedDig := crypto.SHA256(wire[res.UnsignedStart:res.UnsignedEnd])
	if dig != wantUnsignedDig {
		t.Errorf("unsigned digest sanity")
	}
	merkle := MerkleDigestFromBytes(wire[res.UnsignedStart:res.SignedEnd])
	wantMerkle := crypto.SHA256(wire[res.UnsignedStart:res.SignedEnd])
	if merkle != wantMerkle {
		t.Errorf("merkle digest sanity")
	}

	// Re-marshal must produce identical bytes.
	wire2 := AppendSignedTransaction(nil, &res.SignedTx)
	if !bytes.Equal(wire, wire2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", wire2, wire)
	}
}

func TestMerkleRootEmpty(t *testing.T) {
	var zero [20]byte
	if got := MerkleRoot(nil); got != zero {
		t.Fatalf("MerkleRoot([]) = %x, want zeros", got)
	}
}

func TestMerkleRootSingleTransaction(t *testing.T) {
	// Single tx: merkle = ripemd160(its digest).
	txMerkleDigest := DigestFromBytes([]byte("dummy serialized signed_transaction bytes"))
	got := MerkleRoot([]Digest{txMerkleDigest})
	want := crypto.RIPEMD160(txMerkleDigest[:])
	if got != want {
		t.Fatalf("merkle(single tx) mismatch:\n got %x\nwant %x", got, want)
	}
}

func TestMerkleRootTwoTransactions(t *testing.T) {
	d1 := Digest{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}
	d2 := Digest{32, 31, 30, 29, 28, 27, 26, 25, 24, 23, 22, 21, 20, 19, 18, 17,
		16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}

	pair := append(append([]byte{}, d1[:]...), d2[:]...)
	combined := crypto.SHA256(pair)
	want := crypto.RIPEMD160(combined[:])

	got := MerkleRoot([]Digest{d1, d2})
	if got != want {
		t.Fatalf("merkle(2tx) mismatch:\n got %x\nwant %x", got, want)
	}
}

func TestMerkleRootOddCountCarries(t *testing.T) {
	// Three digests: pair (d1,d2) → x; carry d3; then pair (x,d3) → y; final = ripemd160(y).
	d1 := Digest{0x01}
	d2 := Digest{0x02}
	d3 := Digest{0x03}
	x := crypto.SHA256(append(append([]byte{}, d1[:]...), d2[:]...))
	y := crypto.SHA256(append(append([]byte{}, x[:]...), d3[:]...))
	want := crypto.RIPEMD160(y[:])
	got := MerkleRoot([]Digest{d1, d2, d3})
	if got != want {
		t.Fatalf("merkle(3tx odd) mismatch:\n got %x\nwant %x", got, want)
	}
}

// Sanity helper: hex-decoding helper occasionally useful in fixtures.
var _ = hex.EncodeToString
