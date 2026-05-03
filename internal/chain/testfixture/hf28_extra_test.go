package testfixture

import (
	"testing"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// TestDeclinedVotingRightsBasic ports a slice of hf28_tests.cpp::
// declined_voting_rights_basic. After a decline_voting_rights op:
//   - a request is created with effective_date = head + 30 days
//   - rolling forward 30 days runs process_decline_voting_rights
//   - the request is consumed, and the account is marked VoteRevoked
//   - any pending witness votes get cleared and proxy is reset
func TestDeclinedVotingRightsBasic(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF22) // HF22 introduces decline_voting_rights mechanics
	f.Actors("alice", "bob")
	f.FundVesting("alice", 1_000_000)
	f.FundVesting("bob", 1_000_000)
	f.GenerateBlock()

	// Set proxy first.
	f.MustApplyOp(op.AccountWitnessProxy{Account: "alice", Proxy: "bob"})

	// Decline voting rights.
	f.MustApplyOp(op.DeclineVotingRights{Account: "alice", Decline: true})

	// Verify request exists with 30-day effective date.
	req, err := objects.GetDeclineVotingRightsRequest(state.NewSession(f.State), "alice")
	if err != nil {
		t.Fatalf("GetDeclineVotingRightsRequest: %v", err)
	}
	wantEff := f.HeadBlockTime() + 60*60*24*30
	if req.EffectiveDate < wantEff-6 || req.EffectiveDate > wantEff+6 {
		t.Errorf("EffectiveDate: got %d, want ~%d (head+30d)",
			req.EffectiveDate, wantEff)
	}

	// Roll past the 30-day delay — process_decline_voting_rights fires.
	f.SkipSeconds(60 * 60 * 24 * 30)
	f.GenerateBlocks(2)

	// Request should be consumed.
	if has, _ := objects.HasDeclineVotingRightsRequest(state.NewSession(f.State), "alice"); has {
		t.Error("decline request should be consumed after 30 days")
	}

	// Account should be marked VoteRevoked + proxy cleared.
	a, err := objects.GetAccount(state.NewSession(f.State), "alice")
	if err != nil {
		t.Fatalf("GetAccount alice: %v", err)
	}
	if !a.VoteRevoked {
		t.Error("alice.VoteRevoked should be true post-effective")
	}
	if a.Proxy != "" {
		t.Errorf("alice.Proxy should be cleared, got %q", a.Proxy)
	}
}

// TestDeclinedVotingRightsRevoked verifies that an account marked
// VoteRevoked cannot subsequently cast votes (vote evaluator rejects).
func TestDeclinedVotingRightsRevoked(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF22)
	f.Actors("alice", "bob")
	f.FundVesting("alice", 1_000_000)
	f.GenerateBlock()

	f.MustApplyOp(op.DeclineVotingRights{Account: "alice", Decline: true})
	f.SkipSeconds(60 * 60 * 24 * 30)
	f.GenerateBlocks(2)

	// alice should now have VoteRevoked=true. Cast a comment + vote.
	f.MustApplyOp(op.Comment{Author: "bob", Permlink: "p", Body: "x"})

	err := f.ApplyOp(op.Vote{Voter: "alice", Author: "bob", Permlink: "p", Weight: 10000})
	if err == nil {
		t.Error("alice (VoteRevoked) should be rejected when voting on a comment")
	}
}

// TestDeclinedVotingRightsCancelBeforeEffective verifies that decline
// op with decline=false REMOVES a pending request, mirroring the
// "decline=false revokes a previously-issued decline" behavior.
func TestDeclinedVotingRightsCancelBeforeEffective(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF22)
	f.Actors("alice")
	f.GenerateBlock()

	f.MustApplyOp(op.DeclineVotingRights{Account: "alice", Decline: true})
	if has, _ := objects.HasDeclineVotingRightsRequest(state.NewSession(f.State), "alice"); !has {
		t.Fatal("expected pending request after decline=true")
	}

	// Cancel before effective.
	f.MustApplyOp(op.DeclineVotingRights{Account: "alice", Decline: false})
	if has, _ := objects.HasDeclineVotingRightsRequest(state.NewSession(f.State), "alice"); has {
		t.Error("expected request to be cancelled by decline=false")
	}
}

// TestGlobalWitnessPropsHF26HBDPercents ports a slice of
// hf28_tests.cpp::global_witness_props_basic_test (HF26 part). HF26
// raises the HBD start/stop percent to 20% as a global witness prop.
func TestGlobalWitnessPropsHF26HBDPercents(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF26)

	dgpo := f.DGPO()
	if dgpo.HBDStartPercent != 2000 {
		t.Errorf("HF26 HBDStartPercent: got %d, want 2000", dgpo.HBDStartPercent)
	}
	if dgpo.HBDStopPercent != 2000 {
		t.Errorf("HF26 HBDStopPercent: got %d, want 2000", dgpo.HBDStopPercent)
	}
}
