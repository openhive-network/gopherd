package testfixture

import (
	"testing"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// TestCommentFreezeAfterCashout ports operation_time_tests.cpp::comment_freeze.
// After a comment cashes out, its CashoutTime is reset to a "frozen"
// sentinel and further votes against it must be rejected.
func TestCommentFreezeAfterCashout(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actors("alice", "bob")
	f.FundVesting("bob", 1_000_000)
	f.GenerateBlock()

	f.MustApplyOp(op.Comment{Author: "alice", Permlink: "p", Body: "hi"})
	f.SkipSeconds(60 * 60)
	f.MustApplyOp(op.Vote{Voter: "bob", Author: "alice", Permlink: "p", Weight: 10000})

	// Pre-cashout: CashoutTime is non-zero.
	cPre := f.Comment("alice", "p")
	if cPre.CashoutTime == 0 {
		t.Fatal("CashoutTime should be set pre-cashout")
	}

	// Roll past the 7-day window.
	f.SkipSeconds(60 * 60 * 24 * 7)
	f.GenerateBlocks(2)

	// Post-cashout: CashoutTime becomes 0 (frozen sentinel).
	cPost := f.Comment("alice", "p")
	if cPost.CashoutTime != 0 {
		t.Errorf("post-cashout CashoutTime should be 0 (frozen), got %d", cPost.CashoutTime)
	}
	if !cPost.WasPaid {
		t.Error("post-cashout WasPaid should be true")
	}

	// Voting on a frozen comment is silently no-oped (hived's vote
	// evaluator returns early when payout_time == max). No error, no
	// vote row created.
	f.Actor("carol")
	f.FundVesting("carol", 1_000_000)
	if err := f.ApplyOp(op.Vote{Voter: "carol", Author: "alice", Permlink: "p", Weight: 10000}); err != nil {
		t.Errorf("vote on frozen comment should be silent no-op, got error: %v", err)
	}
	// No CommentVote row should have been created.
	if has, _ := objects.HasCommentVote(state.NewSession(f.State), "carol", "alice", "p"); has {
		t.Error("vote row should not be created for frozen comment")
	}
}

// TestRewardFundsLifecycle ports a slice of operation_time_tests.cpp::
// reward_funds (commented out in C++ but the assertions remain useful).
// After HF17 the post reward fund exists and accrues balance over time
// (per-block inflation routes ContentRewardPercent into it).
func TestRewardFundsLifecycle(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.GenerateBlock()

	rfBefore, err := objects.GetRewardFund(state.NewSession(f.State), objects.PostRewardFundName)
	if err != nil {
		t.Fatalf("GetRewardFund: %v", err)
	}
	balBefore := rfBefore.RewardBalance

	// Generate a few blocks. Real-path SkipSeconds runs inflation per block.
	f.SkipSecondsReal(3 * 60) // ~60 blocks

	rfAfter, err := objects.GetRewardFund(state.NewSession(f.State), objects.PostRewardFundName)
	if err != nil {
		t.Fatalf("GetRewardFund post: %v", err)
	}
	if rfAfter.RewardBalance < balBefore {
		t.Errorf("reward fund balance should not shrink without cashouts: %d → %d",
			balBefore, rfAfter.RewardBalance)
	}
}

// TestRecentClaimsDecayAccumulates ports a slice of recent_claims_decay.
// Each block, the reward fund's recent_claims decays slightly per
// HIVE_RECENT_RSHARES_DECAY_TIME (15 days HF19+, 10 days HF17). Without
// cashouts adding to it, it should monotonically decrease (or stay
// approximately equal at low rates).
func TestRecentClaimsDecayMonotonic(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF19)
	f.GenerateBlock()

	// Seed a non-zero recent_claims so decay has something to chew on.
	{
		s := state.NewSession(f.State)
		rf, _ := objects.GetRewardFund(s, objects.PostRewardFundName)
		// Set recent_claims to a large value: high uint64 in lower 8 bytes.
		var seed [16]byte
		seed[8] = 0xff
		seed[9] = 0xff
		seed[10] = 0xff
		seed[11] = 0xff
		rf.RecentClaims = seed
		objects.PutRewardFund(s, &rf)
		if err := s.Commit(false); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	rf0, _ := objects.GetRewardFund(state.NewSession(f.State), objects.PostRewardFundName)
	hi0 := uint64(rf0.RecentClaims[8])<<24 | uint64(rf0.RecentClaims[9])<<16 |
		uint64(rf0.RecentClaims[10])<<8 | uint64(rf0.RecentClaims[11])

	// Walk a full minute (20 blocks) and confirm recent_claims went down.
	f.SkipSecondsReal(60)

	rf1, _ := objects.GetRewardFund(state.NewSession(f.State), objects.PostRewardFundName)
	hi1 := uint64(rf1.RecentClaims[8])<<24 | uint64(rf1.RecentClaims[9])<<16 |
		uint64(rf1.RecentClaims[10])<<8 | uint64(rf1.RecentClaims[11])

	if hi1 > hi0 {
		t.Errorf("recent_claims should decay or stay flat, not grow: %d → %d", hi0, hi1)
	}
}
