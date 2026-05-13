package op

import (
	"bytes"
	"testing"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
)

// TestAllRegularTagsRegistered guards against forgetting to wire up an op
// decoder when adding a new operation. We expect tags 0..49 to all have
// readers — anything missing fails this test.
func TestAllRegularTagsRegistered(t *testing.T) {
	for tag := Tag(0); tag < TagFirstVirtual; tag++ {
		if readers[tag] == nil {
			t.Errorf("tag %d (%s) has no decoder registered", tag, tag.Name())
		}
	}
}

func TestVoteRoundTrip(t *testing.T) {
	v := Vote{Voter: "alice", Author: "bob", Permlink: "hello-world", Weight: 10000}
	wire := AppendOperation(nil, v)
	got, err := ReadOperation(fcraw.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	if got.OpTag() != TagVote {
		t.Fatalf("tag = %d, want %d", got.OpTag(), TagVote)
	}
	v2 := got.(Vote)
	if v2 != v {
		t.Fatalf("round-trip:\n got %+v\nwant %+v", v2, v)
	}
	wire2 := AppendOperation(nil, v2)
	if !bytes.Equal(wire, wire2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", wire2, wire)
	}
}

func TestTransferRoundTrip(t *testing.T) {
	tr := Transfer{
		From:   "alice",
		To:     "bob",
		Amount: asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}},
		Memo:   "hello",
	}
	wire := AppendOperation(nil, tr)
	got, err := ReadOperation(fcraw.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	tr2 := got.(Transfer)
	if tr2.From != tr.From || tr2.To != tr.To || tr2.Amount.Amount != tr.Amount.Amount || tr2.Memo != tr.Memo {
		t.Fatalf("round-trip:\n got %+v\nwant %+v", tr2, tr)
	}
	wire2 := AppendOperation(nil, tr2)
	if !bytes.Equal(wire, wire2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", wire2, wire)
	}
}

// TestLimitOrderCreate2RoundTrip guards the FC_REFLECT field wire order
// for limit_order_create2_operation. C++'s FC_REFLECT
// (libraries/protocol/include/hive/protocol/hive_operations.hpp):
//
//	(owner)(orderid)(amount_to_sell)(exchange_rate)(fill_or_kill)(expiration)
//
// The Go struct lists FillOrKill before ExchangeRate; an earlier port had
// AppendFCBody/readLimitOrderCreate2 follow source order, which caused
// tx-extensions parse-error cascades at blocks 11,875,326 / 11,875,340 /
// 11,889,434.
func TestLimitOrderCreate2RoundTrip(t *testing.T) {
	sell := asset.Asset{Amount: 1234, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}}
	rate := asset.Price{
		Base:  asset.Asset{Amount: 5, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}},
		Quote: asset.Asset{Amount: 1, Symbol: asset.Symbol{Num: asset.HbdSymbol, WasLegacyEncoded: true}},
	}
	o := LimitOrderCreate2{
		Owner:        "alice",
		OrderID:      0xCAFEBABE,
		AmountToSell: sell,
		FillOrKill:   true,
		ExchangeRate: rate,
		Expiration:   1_700_000_000,
	}
	wire := AppendOperation(nil, o)
	got, err := ReadOperation(fcraw.NewReader(wire))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if got.OpTag() != TagLimitOrderCreate2 {
		t.Fatalf("tag = %d, want %d", got.OpTag(), TagLimitOrderCreate2)
	}
	o2 := got.(LimitOrderCreate2)
	if o2.Owner != o.Owner || o2.OrderID != o.OrderID ||
		o2.AmountToSell.Amount != o.AmountToSell.Amount ||
		o2.ExchangeRate.Base.Amount != o.ExchangeRate.Base.Amount ||
		o2.ExchangeRate.Quote.Amount != o.ExchangeRate.Quote.Amount ||
		o2.FillOrKill != o.FillOrKill ||
		o2.Expiration != o.Expiration {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", o2, o)
	}
	wire2 := AppendOperation(nil, o2)
	if !bytes.Equal(wire, wire2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", wire2, wire)
	}

	// Pin the wire byte order so a future field-order regression fails
	// here. After the tag (21 = limit_order_create2), owner string,
	// then orderid u32 (little-endian), then asset (amount_to_sell)
	// THEN price (exchange_rate) — not FillOrKill.
	wantHead := []byte{
		byte(TagLimitOrderCreate2),
		5, 'a', 'l', 'i', 'c', 'e', // owner
		0xBE, 0xBA, 0xFE, 0xCA, // orderid LE
	}
	if !bytes.Equal(wire[:len(wantHead)], wantHead) {
		t.Fatalf("wire-head mismatch:\n got %x\nwant %x", wire[:len(wantHead)], wantHead)
	}
}

// TestEscrowTransferRoundTrip guards the FC_REFLECT field wire order for
// escrow_transfer_operation. The fields in the C++ source
// (libraries/protocol/include/hive/protocol/hive_operations.hpp:1223
// FC_REFLECT) are:
//
//	(from)(to)(hbd_amount)(hive_amount)(escrow_id)(agent)(fee)
//	(json_meta)(ratification_deadline)(escrow_expiration)
//
// The Go struct's source-order is DIFFERENT (it groups account names
// first). A regression here desynchronises Go's wire decoder from
// hived's: the parse runs N bytes past the op's own footprint and into
// the next tx's bytes, surfacing several txs later as a bogus
// `tx.extensions[0]: unsupported variant tag X` error. See commit
// 5f87589 — block 9,284,729 was the first mainnet tx hitting this in
// our test corpus.
func TestEscrowTransferRoundTrip(t *testing.T) {
	hbd := asset.Asset{Amount: 250, Symbol: asset.Symbol{Num: asset.HbdSymbol, WasLegacyEncoded: true}}
	hive := asset.Asset{Amount: 5000, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}}
	fee := asset.Asset{Amount: 10, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}}
	o := EscrowTransfer{
		From:                 "alice",
		To:                   "bob",
		Agent:                "trent",
		EscrowID:             0xDEADBEEF,
		HbdAmount:            hbd,
		HiveAmount:           hive,
		Fee:                  fee,
		RatificationDeadline: 1_700_000_000,
		EscrowExpiration:     1_700_500_000,
		JSONMeta:             `{"note":"contract-42"}`,
	}
	wire := AppendOperation(nil, o)
	got, err := ReadOperation(fcraw.NewReader(wire))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if got.OpTag() != TagEscrowTransfer {
		t.Fatalf("tag = %d, want %d", got.OpTag(), TagEscrowTransfer)
	}
	o2 := got.(EscrowTransfer)
	if o2.From != o.From || o2.To != o.To || o2.Agent != o.Agent ||
		o2.EscrowID != o.EscrowID ||
		o2.HbdAmount.Amount != o.HbdAmount.Amount ||
		o2.HiveAmount.Amount != o.HiveAmount.Amount ||
		o2.Fee.Amount != o.Fee.Amount ||
		o2.RatificationDeadline != o.RatificationDeadline ||
		o2.EscrowExpiration != o.EscrowExpiration ||
		o2.JSONMeta != o.JSONMeta {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", o2, o)
	}
	wire2 := AppendOperation(nil, o2)
	if !bytes.Equal(wire, wire2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", wire2, wire)
	}

	// Explicitly pin the wire byte order so a future change to the
	// Go struct field order (without updating AppendFCBody) fails
	// here, not in production. The first byte is the op tag varint
	// (27 = escrow_transfer), the rest is `from` length-prefixed
	// string, etc.
	wantHead := []byte{
		byte(TagEscrowTransfer),
		5, 'a', 'l', 'i', 'c', 'e', // from
		3, 'b', 'o', 'b', // to
	}
	if !bytes.Equal(wire[:len(wantHead)], wantHead) {
		t.Fatalf("wire-head mismatch:\n got %x\nwant %x", wire[:len(wantHead)], wantHead)
	}
}

func TestCommentOptionsWithBeneficiariesRoundTrip(t *testing.T) {
	o := CommentOptions{
		Author:               "alice",
		Permlink:             "post-1",
		MaxAcceptedPayout:    asset.Asset{Amount: 1_000_000_000, Symbol: asset.Symbol{Num: asset.HbdSymbol, WasLegacyEncoded: true}},
		PercentHbd:           10000,
		AllowVotes:           true,
		AllowCurationRewards: true,
		Beneficiaries: []CommentBeneficiary{
			{Account: "alice", Weight: 5000},
			{Account: "bob", Weight: 2500},
		},
	}
	wire := AppendOperation(nil, o)
	got, err := ReadOperation(fcraw.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	o2 := got.(CommentOptions)
	if len(o2.Beneficiaries) != 2 {
		t.Fatalf("beneficiaries count = %d, want 2", len(o2.Beneficiaries))
	}
	if o2.Beneficiaries[0] != o.Beneficiaries[0] || o2.Beneficiaries[1] != o.Beneficiaries[1] {
		t.Fatalf("beneficiaries mismatch: %+v vs %+v", o2.Beneficiaries, o.Beneficiaries)
	}
	// Re-marshal must produce identical bytes — relies on RawExtensions
	// preservation.
	wire2 := AppendOperation(nil, o2)
	if !bytes.Equal(wire, wire2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", wire2, wire)
	}
}

func TestAccountUpdateOptionalAuthorities(t *testing.T) {
	owner := auth.Authority{WeightThreshold: 1, AccountAuths: []auth.AccountAuth{{Account: "alice", Weight: 1}}}
	o := AccountUpdate{
		Account:      "alice",
		Owner:        &owner,
		Active:       nil,
		Posting:      nil,
		JSONMetadata: `{"v":1}`,
	}
	wire := AppendOperation(nil, o)
	got, err := ReadOperation(fcraw.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	o2 := got.(AccountUpdate)
	if o2.Owner == nil || o2.Owner.WeightThreshold != 1 {
		t.Fatalf("owner roundtrip failed: %+v", o2.Owner)
	}
	if o2.Active != nil || o2.Posting != nil {
		t.Fatalf("absent optionals must round-trip as nil")
	}
	if o2.JSONMetadata != o.JSONMetadata {
		t.Fatalf("json_metadata mismatch")
	}
	wire2 := AppendOperation(nil, o2)
	if !bytes.Equal(wire, wire2) {
		t.Fatalf("re-marshal differs")
	}
}

func TestUnknownTagFails(t *testing.T) {
	// Encode a tag = 200 (well past the regular-op range).
	wire := fcraw.AppendVarUint(nil, 200)
	if _, err := ReadOperation(fcraw.NewReader(wire)); err == nil {
		t.Fatal("expected error for unknown/virtual tag 200")
	}
}
