package testfixture

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
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

// TestDeclinedVotingRightsBasic2 ports hf28_tests.cpp::
// declined_voting_rights_basic_2 (HF27 + HF28). After the
// decline_voting_rights effective date fires, a second
// decline_voting_rights op for the same account must throw because the
// account's can_vote flag is now false. Mirrors hived's
// HIVE_CHAIN_VOTING_ASSERT( account.can_vote ... ) in
// decline_voting_rights_evaluator.
func TestDeclinedVotingRightsBasic2(t *testing.T) {
	for _, tc := range []struct {
		name string
		hf   hardfork.ID
	}{{"HF27", hardfork.HF27}, {"HF28", hardfork.HF28}} {
		hf := tc.hf
		t.Run(tc.name, func(t *testing.T) {
			f := New(t)
			f.HardforksUpTo(hf)
			f.Actor("alice")
			f.FundVesting("alice", 1_000)
			f.GenerateBlock()

			// First decline succeeds.
			f.MustApplyOp(op.DeclineVotingRights{Account: "alice", Decline: true})

			// Roll past the 30-day delay so the request fires.
			f.SkipSeconds(60 * 60 * 24 * 30)
			f.GenerateBlocks(2)

			if !f.Account_("alice").VoteRevoked {
				t.Fatal("alice.VoteRevoked should be true after effective date")
			}

			// Second decline must throw (account.can_vote == false).
			if err := f.ApplyOp(op.DeclineVotingRights{Account: "alice", Decline: true}); err == nil {
				t.Error("expected second decline to be rejected (can_vote=false)")
			}
		})
	}
}

// TestDeclinedVotingRightsProposalVotes ports hf28_tests.cpp::
// declined_voting_rights_proposal_votes. On HF28+, when a decline
// effective date fires, any DHF proposal_votes the account has cast are
// removed. Pre-HF28 the proposal votes survive.
func TestDeclinedVotingRightsProposalVotes(t *testing.T) {
	for _, tc := range []struct {
		name string
		hf   hardfork.ID
	}{{"HF27", hardfork.HF27}, {"HF28", hardfork.HF28}} {
		hf := tc.hf
		t.Run(tc.name, func(t *testing.T) {
			f := New(t)
			f.HardforksUpTo(hf)
			f.Actors("alice", "bob")
			f.GenerateBlock()
			f.FundVesting("alice", 100_000)
			f.FundVesting("bob", 100_000)
			f.FundLiquidHBD("alice", 200_000)
			f.FundLiquidHBD("bob", 200_000)
			f.GenerateBlock()

			// alice declines voting rights BEFORE casting a proposal vote.
			f.MustApplyOp(op.DeclineVotingRights{Account: "alice", Decline: true})
			f.GenerateBlock()

			now := f.HeadBlockTime()
			f.MustApplyOp(op.CreateProposal{
				Creator:   "alice",
				Receiver:  "bob",
				StartDate: now + 60*60*24,
				EndDate:   now + 60*60*24*20,
				DailyPay:  HBD(100),
				Subject:   "x",
				Permlink:  "p",
			})

			f.MustApplyOp(op.UpdateProposalVotes{
				Voter:       "alice",
				ProposalIDs: []int64{0},
				Approve:     true,
			})
			if has, _ := objects.HasProposalVote(state.NewSession(f.State), 0, "alice"); !has {
				t.Fatal("alice's proposal vote should exist pre-effective")
			}
			f.GenerateBlock()

			// Roll past 30-day delay — process_decline_voting_rights fires.
			f.SkipSeconds(60 * 60 * 24 * 30)
			f.GenerateBlocks(2)

			has, _ := objects.HasProposalVote(state.NewSession(f.State), 0, "alice")
			if hf >= hardfork.HF28 {
				if has {
					t.Error("HF28+: proposal vote should be removed by process_decline_voting_rights")
				}
			} else {
				if !has {
					t.Error("pre-HF28: proposal vote should be preserved")
				}
			}
		})
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
