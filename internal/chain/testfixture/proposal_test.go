package testfixture

import (
	"testing"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Port of proposal_tests.cpp::proposal_object_apply (subset).
//
// alice creates a proposal with bob as receiver. The proposal is
// stored, gets a unique id, and the fields match.
func TestProposalObjectApply(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF21)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 100_000)
	f.GenerateBlock()

	startDate := f.HeadBlockTime() + 60*60*24
	endDate := startDate + 60*60*24*20

	if err := f.ApplyOp(op.CreateProposal{
		Creator:   "alice",
		Receiver:  "bob",
		StartDate: startDate,
		EndDate:   endDate,
		DailyPay:  HBD(100),
		Subject:   "hello",
		Permlink:  "permlink",
	}); err != nil {
		t.Fatalf("create_proposal: %v", err)
	}

	// Find the proposal — should be ID 1 (counter starts at 1).
	s := state.NewSession(f.State)
	p, err := objects.GetProposal(s, 0)
	if err != nil {
		t.Fatalf("proposal lookup: %v", err)
	}
	if p.Creator != "alice" {
		t.Fatalf("Creator: got %q, want alice", p.Creator)
	}
	if p.Receiver != "bob" {
		t.Fatalf("Receiver: got %q, want bob", p.Receiver)
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
}

// Port of proposal_tests.cpp::proposal_vote_object_apply (subset).
func TestProposalVoteApply(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF21)
	f.Actors("alice", "bob", "carol")
	f.FundLiquidHBD("alice", 100_000)
	f.FundVesting("carol", 50_000)
	f.GenerateBlock()

	now := f.HeadBlockTime()
	if err := f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "bob",
		StartDate: now + 60*60*24, EndDate: now + 60*60*24*20,
		DailyPay: HBD(100), Subject: "x", Permlink: "p",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Log("--- carol votes for proposal #1")
	if err := f.ApplyOp(op.UpdateProposalVotes{
		Voter:       "carol",
		ProposalIDs: []int64{0},
		Approve:     true,
	}); err != nil {
		t.Fatalf("vote: %v", err)
	}
	has, _ := objects.HasProposalVote(state.NewSession(f.State), 0, "carol")
	if !has {
		t.Fatal("carol's vote should exist")
	}

	t.Log("--- carol unvotes")
	if err := f.ApplyOp(op.UpdateProposalVotes{
		Voter:       "carol",
		ProposalIDs: []int64{0},
		Approve:     false,
	}); err != nil {
		t.Fatalf("unvote: %v", err)
	}
	has, _ = objects.HasProposalVote(state.NewSession(f.State), 0, "carol")
	if has {
		t.Fatal("carol's vote should be removed")
	}

	t.Log("--- voting on non-existent proposal: pre-HF28 silently skips")
	if err := f.ApplyOp(op.UpdateProposalVotes{
		Voter:       "carol",
		ProposalIDs: []int64{99},
		Approve:     true,
	}); err != nil {
		t.Fatalf("pre-HF28 vote on nonexistent proposal must silently skip (C++ dhf_evaluator continue), got %v", err)
	}
	if has, _ := objects.HasProposalVote(state.NewSession(f.State), 99, "carol"); has {
		t.Fatal("skipped vote must not create a row")
	}
}

// Port of proposal_tests.cpp::remove_proposal (subset).
func TestRemoveProposalApply(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF21)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 100_000)
	f.GenerateBlock()

	now := f.HeadBlockTime()
	if err := f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "bob",
		StartDate: now + 60*60*24, EndDate: now + 60*60*24*20,
		DailyPay: HBD(100), Subject: "x", Permlink: "p",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.ApplyOp(op.RemoveProposal{
		ProposalOwner: "alice",
		ProposalIDs:   []int64{0},
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	has, _ := objects.HasProposal(state.NewSession(f.State), 0)
	if has {
		t.Fatal("proposal should be removed")
	}

	t.Log("--- failure removing other's proposal")
	if err := f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "bob",
		StartDate: now + 60*60*24, EndDate: now + 60*60*24*20,
		DailyPay: HBD(100), Subject: "x", Permlink: "p2",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.ApplyOp(op.RemoveProposal{
		ProposalOwner: "bob", // not the creator
		ProposalIDs:   []int64{1},
	}); err == nil {
		t.Fatal("expected ownership failure")
	}
}

// Port of proposal_tests.cpp::update_proposal (HF24+).
func TestUpdateProposalApply(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF24)
	f.Actors("alice", "bob")
	f.FundLiquidHBD("alice", 100_000)
	f.GenerateBlock()

	now := f.HeadBlockTime()
	if err := f.ApplyOp(op.CreateProposal{
		Creator: "alice", Receiver: "bob",
		StartDate: now + 60*60*24, EndDate: now + 60*60*24*20,
		DailyPay: HBD(100), Subject: "old", Permlink: "p",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Log("--- update lowers daily_pay (allowed)")
	if err := f.ApplyOp(op.UpdateProposal{
		ProposalID: 0, Creator: "alice",
		DailyPay: HBD(50), Subject: "new", Permlink: "p",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	p, _ := objects.GetProposal(state.NewSession(f.State), 0)
	if p.DailyPay != 50 {
		t.Fatalf("DailyPay: got %d, want 50", p.DailyPay)
	}
	if p.Subject != "new" {
		t.Fatalf("Subject: got %q, want new", p.Subject)
	}

	t.Log("--- failure raising daily_pay")
	if err := f.ApplyOp(op.UpdateProposal{
		ProposalID: 0, Creator: "alice",
		DailyPay: HBD(200),
	}); err == nil {
		t.Fatal("expected raise-daily-pay failure")
	}
}
