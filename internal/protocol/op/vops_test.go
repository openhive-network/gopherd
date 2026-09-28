package op

import (
	"bytes"
	"testing"

	"gopherd/internal/protocol/asset"
)

// The wire format is the contract: a virtual op body that decodes without
// error but carries fields in the wrong order is silently wrong data, and
// nothing downstream can tell. These pin the two things most likely to drift —
// the tag numbering and the FC_REFLECT field order.

func TestVirtualTagNumbering(t *testing.T) {
	// Virtual ops continue the operation static_variant's numbering. If the
	// regular-op count ever changes, every virtual tag shifts and every
	// consumer of the stream misreads it, so pin the boundary and the ends.
	if TagFirstVirtual != 50 {
		t.Fatalf("TagFirstVirtual = %d, want 50 (last regular op is recurrent_transfer at 49)", TagFirstVirtual)
	}
	cases := []struct {
		tag  Tag
		want Tag
		name string
	}{
		{TagFillConvertRequest, 50, "fill_convert_request"},
		{TagAuthorReward, 51, "author_reward"},
		{TagProducerReward, 64, "producer_reward"},
		{TagProposalFee, 87, "proposal_fee"},
		{TagProxyCleared, 91, "proxy_cleared"},
		{TagDeclinedVotingRights, 92, "declined_voting_rights"},
	}
	for _, c := range cases {
		if c.tag != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.tag, c.want)
		}
	}
	// 43 virtual ops, 50..92 inclusive.
	if TagLastVirtual != 93 {
		t.Errorf("TagLastVirtual = %d, want 93 (one past declined_voting_rights)", TagLastVirtual)
	}
}

// TestFailedRecurrentTransferFieldOrder is the trap worth a dedicated test.
// The FC_REFLECT order is (from)(to)(amount)(memo)(consecutive_failures)
// (remaining_executions)(deleted)(extensions) — memo comes BEFORE
// consecutive_failures — while the C++ CONSTRUCTOR takes consecutive_failures
// before memo. Following the constructor produces a body that decodes cleanly
// and means something else.
func TestFailedRecurrentTransferFieldOrder(t *testing.T) {
	o := FailedRecurrentTransfer{
		From:                "alice",
		To:                  "bob",
		Amount:              asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Memo:                "m",
		ConsecutiveFailures: 7,
		RemainingExecutions: 3,
		Deleted:             true,
	}
	body := o.AppendFCBody(nil)

	// alice(varint 5 + 5) bob(1+3) amount(asset) "m"(1+1) then the scalars.
	memoIdx := bytes.Index(body, []byte{1, 'm'})
	if memoIdx < 0 {
		t.Fatal("memo not found in body")
	}
	after := body[memoIdx+2:]
	if len(after) < 4 {
		t.Fatalf("body too short after memo: %x", after)
	}
	if after[0] != 7 {
		t.Errorf("byte after memo = %d, want consecutive_failures 7 — "+
			"if this is 3 the fields follow the constructor, not FC_REFLECT", after[0])
	}
	if after[1] != 3 || after[2] != 0 {
		t.Errorf("remaining_executions = %d,%d, want 3,0 (uint16 LE)", after[1], after[2])
	}
	if after[3] != 1 {
		t.Errorf("deleted = %d, want 1", after[3])
	}
}

// TestRecurrentTransferPairIDExtension — hived OMITS the extension for pair id
// 0 rather than encoding a zero. The two forms are different bytes, so a
// consumer diffing against hived's stream would see every such op as a
// mismatch.
func TestRecurrentTransferPairIDExtension(t *testing.T) {
	if got := RecurrentTransferPairIDExtension(0); got != nil {
		t.Errorf("pair id 0 must produce no extension, got %x", got)
	}
	// One entry: count=1, variant tag=1, then the id byte.
	if got, want := RecurrentTransferPairIDExtension(5), []byte{1, 1, 5}; !bytes.Equal(got, want) {
		t.Errorf("pair id 5: got %x, want %x", got, want)
	}
}

// TestVirtualOpsCarryTheirTag — AppendOperation writes varint(tag) || body, so
// a vop must report the tag its struct claims. A copy-paste OpTag is invisible
// until a consumer decodes the stream.
func TestVirtualOpsCarryTheirTag(t *testing.T) {
	ops := []Operation{
		ProducerReward{},
		ProxyCleared{},
		DeclinedVotingRights{},
		ExpiredAccountNotification{},
		Hardfork{},
		ProducerMissed{},
		ShutdownWitness{},
		ProposalFee{},
		FillRecurrentTransfer{},
		FailedRecurrentTransfer{},
		AuthorReward{},
		CurationReward{},
		CommentReward{},
		CommentBenefactorReward{},
		CommentPayoutUpdate{},
		Interest{},
		FillVestingWithdraw{},
		FillConvertRequest{},
		FillTransferFromSavings{},
		ReturnVestingDelegation{},
		LiquidityReward{},
		ClearNullAccountBalance{},
		ProposalPay{},
		LimitOrderCancelled{},
	}
	seen := map[Tag]bool{}
	for _, o := range ops {
		tag := o.OpTag()
		if tag < TagFirstVirtual || tag >= TagLastVirtual {
			t.Errorf("%T: tag %d outside the virtual range [%d,%d)", o, tag, TagFirstVirtual, TagLastVirtual)
		}
		if seen[tag] {
			t.Errorf("%T: tag %d already used by another op", o, tag)
		}
		seen[tag] = true

		encoded := AppendOperation(nil, o)
		if len(encoded) == 0 || Tag(encoded[0]) != tag {
			t.Errorf("%T: encoded tag byte = %v, want %d", o, encoded[:1], tag)
		}
	}
	if len(ops) != 24 {
		t.Errorf("wired %d virtual ops; update this count deliberately so the "+
			"remaining 19 stay visible", len(ops))
	}
}
