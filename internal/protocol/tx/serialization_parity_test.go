// Tests ported from hive/tests/unit/tests/serialization_tests.cpp covering
// the signed_transaction binary-round-trip cases. The C++ file uses
// ACTORS()-built fixtures + fc::raw::pack_to_vector / unpack_from_vector;
// Go has no fc::raw but AppendSignedTransaction / ReadSignedTransaction are
// the direct equivalents.
package tx

import (
	"bytes"
	"testing"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
)

// TestSerializationRawRoundTrip ports
// serialization_tests.cpp::serialization_raw_test. The C++ test builds a
// signed_transaction with a single transfer_operation (alice -> bob, 100
// HIVE), packs it, unpacks it, validates, and compares digest(). We do the
// binary round-trip in full (digest equality follows from byte equality).
func TestSerializationRawRoundTrip(t *testing.T) {
	transferOp := op.Transfer{
		From:   "alice",
		To:     "bob",
		Amount: asset.Asset{Amount: 100, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}},
	}
	st := SignedTransaction{
		Transaction: Transaction{
			RefBlockNum:    0,
			RefBlockPrefix: 0,
			Expiration:     1514764800,
			Operations:     op.Ops(transferOp),
		},
	}
	wire := AppendSignedTransaction(nil, &st)
	res, err := ReadSignedTransaction(fcraw.NewReader(wire))
	if err != nil {
		t.Fatalf("decode signed_transaction: %v", err)
	}
	if len(res.SignedTx.Operations) != 1 {
		t.Fatalf("ops count = %d, want 1", len(res.SignedTx.Operations))
	}
	if res.SignedTx.Operations[0].OpTag() != op.TagTransfer {
		t.Fatalf("op[0] tag = %s, want transfer",
			res.SignedTx.Operations[0].OpTag().Name())
	}
	transferBack := res.SignedTx.Operations[0].AsTransfer()
	if transferBack.From != "alice" || transferBack.To != "bob" {
		t.Errorf("from/to mismatch: %s -> %s", transferBack.From, transferBack.To)
	}
	if transferBack.Amount.Amount != 100 || transferBack.Amount.Symbol.Num != asset.HiveSymbol {
		t.Errorf("amount mismatch: %+v", transferBack.Amount)
	}

	// Bit-identical re-marshal.
	wire2 := AppendSignedTransaction(nil, &res.SignedTx)
	if !bytes.Equal(wire, wire2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", wire2, wire)
	}

	// Digest of unsigned range = digest of re-marshalled body.
	dig1 := DigestFromBytes(wire[res.UnsignedStart:res.UnsignedEnd])
	wire2res, err := ReadSignedTransaction(fcraw.NewReader(wire2))
	if err != nil {
		t.Fatalf("decode wire2: %v", err)
	}
	dig2 := DigestFromBytes(wire2[wire2res.UnsignedStart:wire2res.UnsignedEnd])
	if dig1 != dig2 {
		t.Errorf("digest mismatch after round-trip: %x vs %x", dig1, dig2)
	}
}

// TestTransactionObjectPackedRoundTrip ports the spirit of
// serialization_tests.cpp::transaction_object_test. The C++ side packs a
// signed_transaction into a `transaction_object`'s `packed_trx` buffer,
// re-packs the whole object via fc::raw, then unpacks back and checks the
// `packed_trx` byte vector survives untouched. Go has no transaction_object
// chainbase wrapper (replay/sync stream transactions directly), but the
// invariant we care about is: a packed signed_transaction byte buffer can be
// stored, re-read, and equals the original. AppendSignedTransaction +
// ReadSignedTransaction is the direct codec; verify the byte slice survives a
// full encode/decode/encode cycle.
func TestTransactionObjectPackedRoundTrip(t *testing.T) {
	st := SignedTransaction{
		Transaction: Transaction{
			RefBlockNum:    1,
			RefBlockPrefix: 2,
			Expiration:     3,
			Operations: op.Ops(op.Transfer{
				From:   "alice",
				To:     "bob",
				Amount: asset.Asset{Amount: 100, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}},
			}),
		},
	}
	packed := AppendSignedTransaction(nil, &st)

	// Mimic the C++ "store packed_trx, then re-pack the wrapper" pattern by
	// nesting `packed` inside another container — here a varint-length-
	// prefixed byte vector — and verifying the inner bytes survive a
	// decode-then-clone cycle.
	wrapped := fcraw.AppendVarUint(nil, uint32(len(packed)))
	wrapped = append(wrapped, packed...)

	r := fcraw.NewReader(wrapped)
	innerLen, err := r.ReadVarUint()
	if err != nil {
		t.Fatalf("read inner len: %v", err)
	}
	if int(innerLen) != len(packed) {
		t.Fatalf("inner len = %d, want %d", innerLen, len(packed))
	}
	innerStart := r.Pos()
	if err := r.Skip(int(innerLen)); err != nil {
		t.Fatalf("skip: %v", err)
	}
	innerBytes := wrapped[innerStart : innerStart+int(innerLen)]
	if !bytes.Equal(innerBytes, packed) {
		t.Fatalf("inner bytes diverged after wrap/unwrap")
	}

	// Decode the inner packed_trx independently and re-pack — expect
	// byte-identical with the original (the chain-state property the C++
	// test asserts).
	res, err := ReadSignedTransaction(fcraw.NewReader(innerBytes))
	if err != nil {
		t.Fatalf("decode inner: %v", err)
	}
	rePacked := AppendSignedTransaction(nil, &res.SignedTx)
	if !bytes.Equal(rePacked, packed) {
		t.Fatalf("decoded+rePacked differs:\n got %x\nwant %x", rePacked, packed)
	}
}

// TestSerializationJSONRoundTrip ports
// serialization_tests.cpp::serialization_json_test. The C++ side uses
// fc::variant to convert an asset + signed_transaction to JSON and back,
// then compares digests. Go has no fc::variant (the static_variant operation
// JSON form requires a typed tag in the JSON object); the operation-level
// JSON path lives in plugins/api, not in tx/. There's no in-tree codec we
// can drive here, so the case is skipped.
func TestSerializationJSONRoundTrip(t *testing.T) {
	t.Skip("MISSING: fc::variant JSON codec for signed_transaction (no static_variant wrapper in op/)")
}

// TestLegacySignedTransactionJSONParse ports
// serialization_tests.cpp::legacy_signed_transaction. The C++ case uses
// hive::plugins::condenser_api::legacy_signed_transaction (the legacy
// [tag-name, payload] array-form). Go's condenser_api implementation is
// out-of-tree; this case has no codec.
func TestLegacySignedTransactionJSONParse(t *testing.T) {
	t.Skip("MISSING: condenser_api legacy_signed_transaction JSON form")
}

// TestLegacyOperationParse ports
// serialization_tests.cpp::legacy_operation_test. Same blocker as
// TestLegacySignedTransactionJSONParse.
func TestLegacyOperationParse(t *testing.T) {
	t.Skip("MISSING: condenser_api legacy operation JSON form")
}
