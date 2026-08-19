// Tests ported from hive/tests/unit/tests/serialization_tests.cpp. The C++ side
// drives many of these through fc::json::from_string into a static_variant, and
// then checks structural equality via sha256(static_variant). Go uses
// encoding/json (not fc::json) and a typed Op interface (not static_variant), so
// we port the *binary round-trip* spirit: construct the typed Op with the same
// field values, encode via AppendOperation, decode via ReadOperation, and
// require field-for-field equality.
package op

import (
	"bytes"
	"gopherd/internal/protocol/network"
	"testing"

	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/asset"
)

// TestPow2WorkRoundTrip ports serialization_tests.cpp::pow2_work_test. The C++
// test verifies that the same pow2_work static_variant — built from the legacy
// and hf26 JSON forms — hashes identically. Go has no fc::static_variant; we
// instead verify that Pow2 with the equivalent typed payload encodes-decodes
// bit-identically and preserves the input fields:
//
//	{ "input":{"worker_account":"alice","prev_block":"abcdef","nonce":1050},
//	  "pow_summary":0 }
//
// In C++ "abcdef" is a `block_id_type` parsed from hex into a 20-byte buffer:
// the first 4 bytes are the block_num (big-endian) and the rest is the SHA-224
// tail of the signed_block_header. We mirror that here: bytes [0xAB,0xCD,0xEF,0,
// 0,0,0,0,...].
func TestPow2WorkRoundTrip(t *testing.T) {
	// The fixture encodes legacy_chain_properties with the MAINNET legacy asset
	// name (STEEM). A testnet build's reader accepts TESTS instead, so this
	// vector is not decodable there — correctly so.
	if network.Name != "mainnet" {
		t.Skip("mainnet C++ parity vector: testnet legacy asset names differ")
	}
	var prev [20]byte
	prev[0], prev[1], prev[2] = 0xAB, 0xCD, 0xEF // "abcdef" → block_id high bytes

	o := Pow2{
		WorkVariantTag: 0, // pow2 (not equihash_pow)
		PowInput: Pow2Input{
			WorkerAccount: "alice",
			PrevBlock:     prev,
			Nonce:         1050,
		},
		PowSummary:  0,
		NewOwnerKey: nil,
		Props: LegacyChainProperties{
			AccountCreationFee: asset.LegacyHiveAsset{
				Amount: 0,
				Sym:    []byte{0x03, 'S', 'T', 'E', 'E', 'M', 0x00, 0x00},
			},
			MaximumBlockSize: 131072,
			HBDInterestRate:  1000,
		},
	}
	wire := AppendOperation(nil, o)
	got, err := ReadOperation(fcraw.NewReader(wire))
	if err != nil {
		t.Fatalf("decode pow2: %v", err)
	}
	if got.OpTag() != TagPow2 {
		t.Fatalf("tag = %d, want %d", got.OpTag(), TagPow2)
	}
	o2 := got.(Pow2)
	if o2.WorkVariantTag != 0 {
		t.Errorf("WorkVariantTag = %d, want 0", o2.WorkVariantTag)
	}
	if o2.PowInput.WorkerAccount != "alice" {
		t.Errorf("worker_account = %q, want alice", o2.PowInput.WorkerAccount)
	}
	if o2.PowInput.Nonce != 1050 {
		t.Errorf("nonce = %d, want 1050", o2.PowInput.Nonce)
	}
	if o2.PowInput.PrevBlock != prev {
		t.Errorf("prev_block = %x, want %x", o2.PowInput.PrevBlock, prev)
	}
	if o2.PowSummary != 0 {
		t.Errorf("pow_summary = %d, want 0", o2.PowSummary)
	}
	// Round-trip is bit-identical.
	wire2 := AppendOperation(nil, o2)
	if !bytes.Equal(wire, wire2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", wire2, wire)
	}
}

// TestCommentOptionsExtensionRoundTrip ports
// serialization_tests.cpp::comment_options_extension_test. Like the pow2 test,
// the C++ side relies on fc::json + static_variant hashing for the legacy/hf26
// JSON equivalence. We port the binary spirit: build a CommentOptions with a
// single comment_payout_beneficiaries extension naming alice@10000 and verify
// encode-decode preserves the beneficiary list.
func TestCommentOptionsExtensionRoundTrip(t *testing.T) {
	o := CommentOptions{
		Author:               "alice",
		Permlink:             "post-1",
		MaxAcceptedPayout:    asset.Asset{Amount: 1_000_000_000, Symbol: asset.Symbol{Num: asset.HbdSymbol, WasLegacyEncoded: true}},
		PercentHbd:           10000,
		AllowVotes:           true,
		AllowCurationRewards: true,
		Beneficiaries: []CommentBeneficiary{
			{Account: "alice", Weight: 10000},
		},
	}
	wire := AppendOperation(nil, o)
	got, err := ReadOperation(fcraw.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	o2 := got.(CommentOptions)
	if len(o2.Beneficiaries) != 1 {
		t.Fatalf("beneficiaries count = %d, want 1", len(o2.Beneficiaries))
	}
	if o2.Beneficiaries[0].Account != "alice" || o2.Beneficiaries[0].Weight != 10000 {
		t.Fatalf("beneficiary[0] = %+v, want {alice 10000}", o2.Beneficiaries[0])
	}
	wire2 := AppendOperation(nil, o2)
	if !bytes.Equal(wire, wire2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", wire2, wire)
	}
}

// TestCommentOptionsExtensionEmpty verifies that the no-extension case (the
// vast majority of comment_options ops on mainnet) round-trips with an empty
// beneficiaries slice and an extensions count of 0.
func TestCommentOptionsExtensionEmpty(t *testing.T) {
	o := CommentOptions{
		Author:               "alice",
		Permlink:             "post-1",
		MaxAcceptedPayout:    asset.Asset{Amount: 1_000_000_000, Symbol: asset.Symbol{Num: asset.HbdSymbol, WasLegacyEncoded: true}},
		PercentHbd:           10000,
		AllowVotes:           true,
		AllowCurationRewards: true,
	}
	wire := AppendOperation(nil, o)
	got, err := ReadOperation(fcraw.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	o2 := got.(CommentOptions)
	if len(o2.Beneficiaries) != 0 {
		t.Fatalf("beneficiaries count = %d, want 0", len(o2.Beneficiaries))
	}
	wire2 := AppendOperation(nil, o2)
	if !bytes.Equal(wire, wire2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", wire2, wire)
	}
}
