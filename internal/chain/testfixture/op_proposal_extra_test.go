package testfixture

// Additional proposal-evaluator tests ported from
// hive/tests/unit/tests/proposal_tests.cpp (the 12 simple, portable
// cases — heavyweight maintenance/converting tests are out of scope).
//
// Naming convention: the test function name closes with the C++ case
// name (e.g. ...CreateProposal000) so a reader can grep across both
// codebases. Some cases overlap with TestCreateProposal* in
// op_proposal_validate_test.go and TestProposal{Object,Vote,Remove,Update}*Apply
// in proposal_test.go; in those cases this file adds a faithful
// 1:1 port — the C++ helper builds the op slightly differently
// (post_comment_with_block_generation, set_price_feed, etc.) but for
// the evaluator-level checks the behavior is the same.

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// helper that bumps to HF21 (DHF activation) and funds alice with
// enough HBD to cover one or more proposal-creation treasury fees.
func dhfFixture(t *testing.T, extraActors ...string) *Fixture {
	t.Helper()
	f := New(t)
	actors := append([]string{"alice", "bob"}, extraActors...)
	f.Actors(actors...)
	f.FundLiquidHBD("alice", 80_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF21)
	return f
}

// dhfFixtureHF24 is the HF24-active variant used by the fee-increase
// test (HF24 enables runtime-scaled fees) and update_proposal_* tests
// (update_proposal op activates at HF24).
//
// Note we fund AFTER bumping to HF24. Otherwise the HardforksUpTo jump
// advances head_block_time by years, the hbd_seconds counter racks up
// huge accrued interest, and the next AdjustHBDBalance call pays it
// out — distorting the fee-delta measurement.
func dhfFixtureHF24(t *testing.T, extraActors ...string) *Fixture {
	t.Helper()
	f := New(t)
	actors := append([]string{"alice", "bob"}, extraActors...)
	f.Actors(actors...)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF24)
	f.FundLiquidHBD("alice", 1_000_000)
	f.GenerateBlock()
	return f
}

// findProposalIDByCreator returns the first proposal_id with the given
// creator, or -1 if none. Mirrors C++'s
// `proposal_idx.find( by_creator, creator )`.
func findProposalIDByCreator(f *Fixture, creator string) int64 {
	var pid int64 = -1
	_ = state.NewSession(f.State).Range(objects.ProposalTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			p, err := objects.ReadProposal(value)
			if err == nil && p.Creator == creator && pid < 0 {
				pid = p.ID
			}
			return true, nil
		})
	return pid
}

func countProposalsByCreator(f *Fixture, creator string) int {
	n := 0
	_ = state.NewSession(f.State).Range(objects.ProposalTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			p, err := objects.ReadProposal(value)
			if err == nil && p.Creator == creator {
				n++
			}
			return true, nil
		})
	return n
}

// TestProposalObjectApplyFeeIncrease_cpp ports
// proposal_tests.cpp::proposal_object_apply_fee_increase. A 90-day
// proposal under HF24 pays treasury_fee + (90 - 60) * fee_increase_amount.
//
// fee = 10 HBD + 30 * 1 HBD = 40 HBD = 40000 sat-HBD.
func TestProposalObjectApplyFeeIncrease_cpp(t *testing.T) {
	f := dhfFixtureHF24(t)

	creator := "alice"
	receiver := "bob"

	startDate := f.HeadBlockTime() + 60*60*24 // +1d
	endDate := startDate + 60*60*24*90        // +90d
	expectedFee := int64(10_000 + 30*1_000)   // 10 HBD + 30 extra-day HBDs

	beforeCreator := f.HBDBalance(creator)
	beforeReceiver := f.HBDBalance(receiver)
	treasuryName := "hive.fund"
	beforeTreasury := func() int64 {
		acc, err := objects.GetAccount(state.NewSession(f.State), treasuryName)
		if err != nil {
			return 0
		}
		return acc.HBDBalance
	}()

	f.MustApplyOp(op.CreateProposal{
		Creator:   creator,
		Receiver:  receiver,
		StartDate: startDate,
		EndDate:   endDate,
		DailyPay:  HBD(100),
		Subject:   "hello",
		Permlink:  "somethingpermlink",
	})

	afterCreator := f.HBDBalance(creator)
	afterReceiver := f.HBDBalance(receiver)
	afterTreasury := func() int64 {
		acc, _ := objects.GetAccount(state.NewSession(f.State), treasuryName)
		return acc.HBDBalance
	}()

	if got := beforeCreator - afterCreator; got != expectedFee {
		t.Fatalf("creator HBD debit: got %d, want %d", got, expectedFee)
	}
	if afterReceiver != beforeReceiver {
		t.Fatalf("receiver HBD changed unexpectedly: before=%d after=%d", beforeReceiver, afterReceiver)
	}
	if got := afterTreasury - beforeTreasury; got != expectedFee {
		t.Fatalf("treasury HBD credit: got %d, want %d", got, expectedFee)
	}

	pid := findProposalIDByCreator(f, creator)
	if pid < 0 {
		t.Fatalf("proposal not stored under creator=%q", creator)
	}
	p, err := objects.GetProposal(state.NewSession(f.State), pid)
	if err != nil {
		t.Fatalf("proposal lookup: %v", err)
	}
	if p.Creator != creator {
		t.Fatalf("Creator: got %q, want %q", p.Creator, creator)
	}
	if p.Receiver != receiver {
		t.Fatalf("Receiver: got %q, want %q", p.Receiver, receiver)
	}
	if p.StartDate != startDate {
		t.Fatalf("StartDate: got %d, want %d", p.StartDate, startDate)
	}
	if p.EndDate != endDate {
		t.Fatalf("EndDate: got %d, want %d", p.EndDate, endDate)
	}
	if p.DailyPay != 100 {
		t.Fatalf("DailyPay: got %d, want 100", p.DailyPay)
	}
	if p.Subject != "hello" {
		t.Fatalf("Subject: got %q, want hello", p.Subject)
	}
	if p.Permlink != "somethingpermlink" {
		t.Fatalf("Permlink: got %q, want somethingpermlink", p.Permlink)
	}
}

// TestProposalCreate000_cpp ports create_proposal_000 — happy path.
func TestProposalCreate000_cpp(t *testing.T) {
	f := dhfFixture(t)

	startDate := f.HeadBlockTime() + 60*60*24
	endDate := startDate + 60*60*24*2
	f.MustApplyOp(op.CreateProposal{
		Creator:   "alice",
		Receiver:  "bob",
		StartDate: startDate,
		EndDate:   endDate,
		DailyPay:  HBD(100),
		Subject:   "subject",
		Permlink:  "permlink",
	})

	if pid := findProposalIDByCreator(f, "alice"); pid < 0 {
		t.Fatalf("expected a proposal owned by alice")
	}
}

// TestProposalCreate001_cpp ports create_proposal_001 — empty creator
// name rejected. The Validate gate catches this regardless of HF.
func TestProposalCreate001_cpp(t *testing.T) {
	f := dhfFixture(t)

	now := f.HeadBlockTime()
	err := f.ApplyOp(op.CreateProposal{
		Creator:   "",
		Receiver:  "bob",
		StartDate: now + 60*60*24,
		EndDate:   now + 60*60*24*3,
		DailyPay:  HBD(100),
		Subject:   "subject",
		Permlink:  "permlink",
	})
	if err == nil {
		t.Fatal("create_proposal with empty creator should fail")
	}
}

// TestProposalCreate002_cpp ports create_proposal_002 — empty receiver.
func TestProposalCreate002_cpp(t *testing.T) {
	f := dhfFixture(t)

	now := f.HeadBlockTime()
	err := f.ApplyOp(op.CreateProposal{
		Creator:   "alice",
		Receiver:  "",
		StartDate: now + 60*60*24,
		EndDate:   now + 60*60*24*3,
		DailyPay:  HBD(100),
		Subject:   "subject",
		Permlink:  "permlink",
	})
	if err == nil {
		t.Fatal("create_proposal with empty receiver should fail")
	}
}

// TestProposalCreate003_cpp ports create_proposal_003 — start_date >
// end_date is rejected (start placed +2d past the end).
func TestProposalCreate003_cpp(t *testing.T) {
	f := dhfFixture(t)

	end := f.HeadBlockTime() + 60*60*24
	start := end + 60*60*24*2
	err := f.ApplyOp(op.CreateProposal{
		Creator:   "alice",
		Receiver:  "bob",
		StartDate: start,
		EndDate:   end,
		DailyPay:  HBD(100),
		Subject:   "subject",
		Permlink:  "permlink",
	})
	if err == nil {
		t.Fatal("create_proposal with start_date > end_date should fail")
	}
}

// TestProposalCreate004_cpp ports create_proposal_004 — end_date <
// start_date (here: end pushed 2d before start).
func TestProposalCreate004_cpp(t *testing.T) {
	f := dhfFixture(t)

	start := f.HeadBlockTime() + 60*60*24*5
	end := start - 60*60*24*2
	err := f.ApplyOp(op.CreateProposal{
		Creator:   "alice",
		Receiver:  "bob",
		StartDate: start,
		EndDate:   end,
		DailyPay:  HBD(100),
		Subject:   "subject",
		Permlink:  "permlink",
	})
	if err == nil {
		t.Fatal("create_proposal with end_date < start_date should fail")
	}
}

// TestProposalCreate005_cpp ports create_proposal_005 — empty subject.
func TestProposalCreate005_cpp(t *testing.T) {
	f := dhfFixture(t)

	now := f.HeadBlockTime()
	err := f.ApplyOp(op.CreateProposal{
		Creator:   "alice",
		Receiver:  "bob",
		StartDate: now + 60*60*24,
		EndDate:   now + 60*60*24*3,
		DailyPay:  HBD(100),
		Subject:   "",
		Permlink:  "http:://something.html",
	})
	if err == nil {
		t.Fatal("create_proposal with empty subject should fail")
	}
}

// TestProposalUpdate001_cpp ports update_proposal_001 — looking up an
// id that doesn't exist (50) and a negative id (-50) both fail.
func TestProposalUpdate001_cpp(t *testing.T) {
	f := dhfFixtureHF24(t)

	startDate := f.HeadBlockTime() + 60*60*24
	endDate := startDate + 60*60*24*2
	f.MustApplyOp(op.CreateProposal{
		Creator:   "alice",
		Receiver:  "bob",
		StartDate: startDate,
		EndDate:   endDate,
		DailyPay:  HBD(100),
		Subject:   "hello",
		Permlink:  "somethingpermlink",
	})

	// Non-existent id.
	if err := f.ApplyOp(op.UpdateProposal{
		ProposalID: 50,
		Creator:    "alice",
		DailyPay:   HBD(100),
		Subject:    "hello",
		Permlink:   "somethingpermlink",
	}); err == nil {
		t.Fatal("update_proposal with id=50 (missing) should fail")
	}

	// Negative id.
	if err := f.ApplyOp(op.UpdateProposal{
		ProposalID: -50,
		Creator:    "alice",
		DailyPay:   HBD(100),
		Subject:    "hello",
		Permlink:   "somethingpermlink",
	}); err == nil {
		t.Fatal("update_proposal with id=-50 should fail")
	}
}

// TestProposalUpdate002_cpp ports update_proposal_002 — only the
// proposal owner may update.
func TestProposalUpdate002_cpp(t *testing.T) {
	f := dhfFixtureHF24(t)

	startDate := f.HeadBlockTime() + 60*60*24
	endDate := startDate + 60*60*24*2
	f.MustApplyOp(op.CreateProposal{
		Creator:   "alice",
		Receiver:  "bob",
		StartDate: startDate,
		EndDate:   endDate,
		DailyPay:  HBD(100),
		Subject:   "hello",
		Permlink:  "somethingpermlink",
	})

	pid := findProposalIDByCreator(f, "alice")
	if pid < 0 {
		t.Fatal("expected proposal created by alice")
	}

	if err := f.ApplyOp(op.UpdateProposal{
		ProposalID: pid,
		Creator:    "bob", // not the owner
		DailyPay:   HBD(100),
		Subject:    "hello",
		Permlink:   "somethingpermlink",
	}); err == nil {
		t.Fatal("update_proposal by non-owner should fail")
	}
}

// TestProposalRemove000_cpp ports remove_proposal_000 — single
// proposal, removed by owner, row is gone.
func TestProposalRemove000_cpp(t *testing.T) {
	f := dhfFixture(t)

	startDate := f.HeadBlockTime() + 60*60*24
	endDate := startDate + 60*60*24*2
	f.MustApplyOp(op.CreateProposal{
		Creator:   "alice",
		Receiver:  "bob",
		StartDate: startDate,
		EndDate:   endDate,
		DailyPay:  HBD(100),
		Subject:   "subject",
		Permlink:  "permlink",
	})

	pid := findProposalIDByCreator(f, "alice")
	if pid < 0 {
		t.Fatal("expected proposal owned by alice")
	}
	if got := countProposalsByCreator(f, "alice"); got != 1 {
		t.Fatalf("count before remove: got %d, want 1", got)
	}

	f.MustApplyOp(op.RemoveProposal{
		ProposalOwner: "alice",
		ProposalIDs:   []int64{pid},
	})

	if has, _ := objects.HasProposal(state.NewSession(f.State), pid); has {
		t.Fatalf("proposal %d should be gone", pid)
	}
	if got := countProposalsByCreator(f, "alice"); got != 0 {
		t.Fatalf("count after remove: got %d, want 0", got)
	}
}

// TestProposalRemove001_cpp ports remove_proposal_001 — three
// proposals are created and removed one at a time; the counts after
// each step are 3 -> 2 -> 1 -> 0.
func TestProposalRemove001_cpp(t *testing.T) {
	f := dhfFixture(t)

	startDate := f.HeadBlockTime() + 60*60*24
	endDate := startDate + 60*60*24*2
	mkCreate := func(permlink string) op.CreateProposal {
		return op.CreateProposal{
			Creator:   "alice",
			Receiver:  "bob",
			StartDate: startDate,
			EndDate:   endDate,
			DailyPay:  HBD(100),
			Subject:   "subject",
			Permlink:  permlink,
		}
	}
	f.MustApplyOp(mkCreate("p1"))
	f.MustApplyOp(mkCreate("p2"))
	f.MustApplyOp(mkCreate("p3"))

	if got := countProposalsByCreator(f, "alice"); got != 3 {
		t.Fatalf("initial count: got %d, want 3", got)
	}

	// Collect the three proposal ids in any order; remove them one at
	// a time and assert the residual count.
	var ids []int64
	_ = state.NewSession(f.State).Range(objects.ProposalTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			p, err := objects.ReadProposal(value)
			if err == nil && p.Creator == "alice" {
				ids = append(ids, p.ID)
			}
			return true, nil
		})
	if len(ids) != 3 {
		t.Fatalf("expected 3 ids, got %d", len(ids))
	}

	for i, id := range ids {
		f.MustApplyOp(op.RemoveProposal{
			ProposalOwner: "alice",
			ProposalIDs:   []int64{id},
		})
		want := 3 - (i + 1)
		if got := countProposalsByCreator(f, "alice"); got != want {
			t.Fatalf("after removing id=%d (step %d): count=%d, want %d", id, i+1, got, want)
		}
	}
}

// TestProposalVoteObjectApply_cpp ports proposal_vote_object_apply —
// faithful 1:1 with the C++ case (alice/bob/carol/dan ACTORS; carol
// votes then unvotes a single proposal).
func TestProposalVoteObjectApply_cpp(t *testing.T) {
	f := dhfFixture(t, "carol", "dan")
	f.FundVesting("carol", 1_000_000)
	f.GenerateBlock()

	startDate := f.HeadBlockTime() + 60*60*24
	endDate := startDate + 60*60*24*2

	f.MustApplyOp(op.CreateProposal{
		Creator:   "alice",
		Receiver:  "bob",
		StartDate: startDate,
		EndDate:   endDate,
		DailyPay:  HBD(100),
		Subject:   "subject",
		Permlink:  "permlink",
	})
	pid := findProposalIDByCreator(f, "alice")
	if pid < 0 {
		t.Fatal("expected created proposal")
	}

	// carol approves.
	f.MustApplyOp(op.UpdateProposalVotes{
		Voter:       "carol",
		ProposalIDs: []int64{pid},
		Approve:     true,
	})
	if has, _ := objects.HasProposalVote(state.NewSession(f.State), pid, "carol"); !has {
		t.Fatal("carol's vote should exist after approve")
	}

	// carol unvotes.
	f.MustApplyOp(op.UpdateProposalVotes{
		Voter:       "carol",
		ProposalIDs: []int64{pid},
		Approve:     false,
	})
	if has, _ := objects.HasProposalVote(state.NewSession(f.State), pid, "carol"); has {
		t.Fatal("carol's vote should be gone after disapprove")
	}
}

// TestProposalVoteObject01Apply_cpp ports proposal_vote_object_01_apply —
// two voters (carol, dan) toggle approval across three proposals,
// and we verify the per-voter row count after each step.
//
// The C++ test exercises: approve {0,1}, approve {0,1,2}, re-approve {0}
// (no-op), unapprove {2}, unapprove {0}, unapprove all dan, unapprove {1}.
func TestProposalVoteObject01Apply_cpp(t *testing.T) {
	f := dhfFixture(t, "carol", "dan")
	f.FundVesting("carol", 1_000_000)
	f.FundVesting("dan", 1_000_000)
	f.GenerateBlock()

	startDate := f.HeadBlockTime() + 60*60*24
	endDate := startDate + 60*60*24*2

	mkCreate := func(permlink string, dailyPay int64) op.CreateProposal {
		return op.CreateProposal{
			Creator:   "alice",
			Receiver:  "bob",
			StartDate: startDate,
			EndDate:   endDate,
			DailyPay:  HBD(dailyPay),
			Subject:   "subject",
			Permlink:  permlink,
		}
	}

	// Create p0 and p1, vote carol → {p0,p1}.
	f.MustApplyOp(mkCreate("p0", 100))
	f.MustApplyOp(mkCreate("p1", 101))

	// Collect ids in stable order via ID ascending.
	var allIDs []int64
	_ = state.NewSession(f.State).Range(objects.ProposalTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			p, err := objects.ReadProposal(value)
			if err == nil && p.Creator == "alice" {
				allIDs = append(allIDs, p.ID)
			}
			return true, nil
		})
	if len(allIDs) != 2 {
		t.Fatalf("expected 2 proposals at this point, got %d", len(allIDs))
	}
	p0, p1 := allIDs[0], allIDs[1]

	// HasProposalVote is the cheap, key-correct probe — count by
	// asking it for each known proposal id.
	countVotes := func(voter string, ids []int64) int {
		n := 0
		s := state.NewSession(f.State)
		for _, id := range ids {
			if has, _ := objects.HasProposalVote(s, id, voter); has {
				n++
			}
		}
		return n
	}

	f.MustApplyOp(op.UpdateProposalVotes{
		Voter:       "carol",
		ProposalIDs: []int64{p0, p1},
		Approve:     true,
	})
	if got := countVotes("carol", []int64{p0, p1}); got != 2 {
		t.Fatalf("carol votes after approve {p0,p1}: got %d, want 2", got)
	}

	// Create p2; dan approves all three.
	f.MustApplyOp(mkCreate("p2", 102))
	allIDs = allIDs[:0]
	_ = state.NewSession(f.State).Range(objects.ProposalTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			p, err := objects.ReadProposal(value)
			if err == nil && p.Creator == "alice" {
				allIDs = append(allIDs, p.ID)
			}
			return true, nil
		})
	if len(allIDs) != 3 {
		t.Fatalf("expected 3 proposals at this point, got %d", len(allIDs))
	}
	p2 := allIDs[2]

	f.MustApplyOp(op.UpdateProposalVotes{
		Voter:       "dan",
		ProposalIDs: []int64{p0, p1, p2},
		Approve:     true,
	})
	if got := countVotes("dan", []int64{p0, p1, p2}); got != 3 {
		t.Fatalf("dan votes after approve {p0,p1,p2}: got %d, want 3", got)
	}

	// dan re-approves {p0} — no-op; still 3 votes.
	f.MustApplyOp(op.UpdateProposalVotes{
		Voter:       "dan",
		ProposalIDs: []int64{p0},
		Approve:     true,
	})
	if got := countVotes("dan", []int64{p0, p1, p2}); got != 3 {
		t.Fatalf("dan votes after re-approve {p0}: got %d, want 3", got)
	}

	// carol unvotes p2 — she never voted for p2 → still 2.
	f.MustApplyOp(op.UpdateProposalVotes{
		Voter:       "carol",
		ProposalIDs: []int64{p2},
		Approve:     false,
	})
	if got := countVotes("carol", []int64{p0, p1, p2}); got != 2 {
		t.Fatalf("carol votes after unvote p2: got %d, want 2", got)
	}

	// carol unvotes p0 — was approved → 1 remaining.
	f.MustApplyOp(op.UpdateProposalVotes{
		Voter:       "carol",
		ProposalIDs: []int64{p0},
		Approve:     false,
	})
	if got := countVotes("carol", []int64{p0, p1, p2}); got != 1 {
		t.Fatalf("carol votes after unvote p0: got %d, want 1", got)
	}

	// dan unvotes all three at once → 0.
	f.MustApplyOp(op.UpdateProposalVotes{
		Voter:       "dan",
		ProposalIDs: []int64{p0, p1, p2},
		Approve:     false,
	})
	if got := countVotes("dan", []int64{p0, p1, p2}); got != 0 {
		t.Fatalf("dan votes after unvote {p0,p1,p2}: got %d, want 0", got)
	}

	// carol unvotes p1 → 0.
	f.MustApplyOp(op.UpdateProposalVotes{
		Voter:       "carol",
		ProposalIDs: []int64{p1},
		Approve:     false,
	})
	if got := countVotes("carol", []int64{p0, p1, p2}); got != 0 {
		t.Fatalf("carol votes after unvote p1: got %d, want 0", got)
	}

	// Voting/unvoting an empty set must fail (validate rejects it).
	if err := f.ApplyOp(op.UpdateProposalVotes{
		Voter:       "carol",
		ProposalIDs: []int64{},
		Approve:     true,
	}); err == nil {
		t.Fatal("empty approve list should be rejected")
	}
	if err := f.ApplyOp(op.UpdateProposalVotes{
		Voter:       "carol",
		ProposalIDs: []int64{},
		Approve:     false,
	}); err == nil {
		t.Fatal("empty unapprove list should be rejected")
	}
}
