package objects

import (
	"testing"

	"hived-go/internal/protocol/auth"
)

// Cold-tier read microbenchmark. The 48M+ replay slowdown is dominated by
// per-cold-read CPU: the CPU profile attributes ~25% to ReadComment
// (deserialize) and the ~30% GC to the strings + beneficiary slice it
// allocates on every cold read (see project_replay_speed_profile_2026_07_01).
//
// This benchmark is the FAST measurement loop for that fix direction — it
// runs in seconds and reports ns/op + allocs/op + B/op, so a zero-copy /
// lower-alloc ReadComment can be A/B tested WITHOUT a 15-min snapshot load.
//
//	go test ./internal/state/objects/ -run=NONE -bench=ReadComment -benchmem
//
// Two fixtures span the realistic range: a bare root post (no beneficiaries,
// the common case) and a reply with the max beneficiary set (alloc-heavy).

func benchComment(withBenef bool) *Comment {
	c := &Comment{
		Author:               auth.AccountName("someguy123"),
		Permlink:             "a-fairly-typical-hive-permlink-2026-07-01",
		ParentAuthor:         "",
		ParentPermlink:       "hive-blog",
		Created:              1_600_000_000,
		LastUpdate:           1_600_000_100,
		CashoutTime:          1_600_600_000,
		NetRshares:           123_456_789_012,
		VoteCount:            417,
		MaxAcceptedPayout:    1_000_000_000,
		PercentHbd:           10000,
		AllowVotes:           true,
		AllowCurationRewards: true,
		AbsRshares:           987_654_321_098,
		WasPaid:              true,
		VoteRshares:          123_456_789_012,
		TotalVoteWeight:      9_876_543_210,
		LastPayout:           1_600_600_050,
		RootAuthor:           auth.AccountName("someguy123"),
		RootPermlink:         "a-fairly-typical-hive-permlink-2026-07-01",
		Children:             12,
		ID:                   42_000_000,
		ChildrenAbsRshares:   55_555_555_555,
		MaxCashoutTime:       0xFFFFFFFF,
		RewardWeight:         10000,
		WasVotedOn:           true,
	}
	if withBenef {
		// A reply into another thread with the max beneficiary set.
		c.ParentAuthor = auth.AccountName("blocktrades")
		c.RootAuthor = auth.AccountName("blocktrades")
		c.RootPermlink = "the-root-post-of-a-deep-thread"
		c.Beneficiaries = []Beneficiary{
			{Account: "steemcleaners", Weight: 500},
			{Account: "hivewatchers", Weight: 500},
			{Account: "spaminator", Weight: 1000},
			{Account: "buildawhale", Weight: 2500},
		}
	}
	return c
}

func benchmarkReadComment(b *testing.B, withBenef bool) {
	buf := AppendComment(nil, benchComment(withBenef))
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := ReadComment(buf)
		if err != nil {
			b.Fatal(err)
		}
		// Touch a field so the compiler can't elide the decode.
		if c.ID == 0 {
			b.Fatal("unexpected zero ID")
		}
	}
}

func BenchmarkReadCommentRoot(b *testing.B)  { benchmarkReadComment(b, false) }
func BenchmarkReadCommentBenef(b *testing.B) { benchmarkReadComment(b, true) }
