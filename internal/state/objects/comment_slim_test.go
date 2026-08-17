package objects

import (
	"testing"

	"gopherd/internal/protocol/auth"
)

func TestCommentSlimRoundTrip(t *testing.T) {
	frozen := &Comment{
		Author: "alice", Permlink: "post-1",
		ParentAuthor: "", ParentPermlink: "hive-1",
		RootAuthor: "alice", RootPermlink: "post-1",
		Created: 111, LastUpdate: 222, CashoutTime: 0,
		Children: 3, ID: 42, MaxAcceptedPayout: 1_000_000_000,
		PercentHbd: 10000, AllowVotes: true, AllowCurationRewards: true,
		WasVotedOn: true, WasPaid: true,
		// heavy fields that MUST be dropped:
		NetRshares: 999, AbsRshares: 888, VoteRshares: 777, TotalVoteWeight: 666,
		VoteCount: 55, LastPayout: 44, ChildrenAbsRshares: 33, MaxCashoutTime: 22,
		RewardWeight: 11, Beneficiaries: []Beneficiary{{Account: "bob", Weight: 500}},
	}
	if !commentFrozenForSlim(frozen) {
		t.Fatal("frozen comment should be slim-eligible")
	}
	blob := AppendCommentSlim(nil, frozen)
	if blob[0] != commentSlimSentinel {
		t.Fatalf("slim blob must start with sentinel, got %d", blob[0])
	}
	got, err := ReadComment(blob)
	if err != nil {
		t.Fatal(err)
	}
	// Permanent fields preserved.
	if got.Author != "alice" || got.Permlink != "post-1" || got.RootAuthor != "alice" ||
		got.RootPermlink != "post-1" || got.Created != 111 || got.CashoutTime != 0 ||
		got.Children != 3 || got.ID != 42 || got.MaxAcceptedPayout != 1_000_000_000 ||
		got.PercentHbd != 10000 || !got.AllowVotes || !got.AllowCurationRewards ||
		!got.WasVotedOn || !got.WasPaid {
		t.Fatalf("permanent field lost: %+v", got)
	}
	// Heavy fields dropped (zero-value).
	if got.NetRshares != 0 || got.AbsRshares != 0 || got.VoteRshares != 0 ||
		got.TotalVoteWeight != 0 || got.VoteCount != 0 || got.LastPayout != 0 ||
		got.ChildrenAbsRshares != 0 || got.MaxCashoutTime != 0 || got.RewardWeight != 0 ||
		len(got.Beneficiaries) != 0 {
		t.Fatalf("heavy field survived slim: %+v", got)
	}
	// routing peek works on slim blobs.
	a, p, ra, rp, wasPaid, ok := peekCommentSpillRouting(blob)
	if !ok || a != "alice" || p != "post-1" || ra != "alice" || rp != "post-1" || !wasPaid {
		t.Fatalf("peek slim routing wrong: a=%q p=%q ra=%q rp=%q paid=%v ok=%v", a, p, ra, rp, wasPaid, ok)
	}
	// A live (unpaid) comment is NOT slimmed.
	live := &Comment{Author: "x", Permlink: "y", CashoutTime: 5000, WasPaid: false}
	if commentFrozenForSlim(live) {
		t.Fatal("live comment must not be slim-eligible")
	}
	// A pre-HF17 comment between cashouts (WasPaid but CashoutTime != max) not slimmed.
	between := &Comment{Author: "x", Permlink: "y", CashoutTime: 6000, WasPaid: true}
	if commentFrozenForSlim(between) {
		t.Fatal("between-cashouts comment must not be slim-eligible")
	}
	_ = auth.AccountName("")
}
