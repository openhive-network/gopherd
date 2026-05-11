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
	v2 := got.(*Vote)
	if *v2 != v {
		t.Fatalf("round-trip:\n got %+v\nwant %+v", *v2, v)
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
