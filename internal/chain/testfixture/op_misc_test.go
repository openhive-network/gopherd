package testfixture

import (
	"testing"

	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Port of operation_tests.cpp::decline_voting_rights_apply.
//
// alice declines voting rights. After HIVE_OWNER_AUTH_RECOVERY_PERIOD
// (30 days), the per-block hook clears her witness votes and sets
// VoteRevoked. Subsequent witness votes should fail.
func TestDeclineVotingRightsApply(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()
	f.FundVesting("alice", 10_000)
	f.FundVesting("bob", 10_000)
	f.GenerateBlock()

	t.Log("--- Success creating decline request")
	if err := f.ApplyOp(op.DeclineVotingRights{
		Account: "alice",
		Decline: true,
	}); err != nil {
		t.Fatalf("decline: %v", err)
	}
	s := state.NewSession(f.State)
	req, err := objects.GetDeclineVotingRightsRequest(s, "alice")
	if err != nil {
		t.Fatalf("expected request to exist: %v", err)
	}
	expected := f.HeadBlockTimeForApply() + 60*60*24*30
	if req.EffectiveDate != expected {
		t.Fatalf("effective date: got %d, want %d", req.EffectiveDate, expected)
	}

	t.Log("--- Failure creating duplicate request")
	if err := f.ApplyOp(op.DeclineVotingRights{
		Account: "alice",
		Decline: true,
	}); err == nil {
		t.Fatal("expected duplicate-request failure")
	}

	t.Log("--- Cancelling the request")
	if err := f.ApplyOp(op.DeclineVotingRights{
		Account: "alice",
		Decline: false,
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if has, _ := objects.HasDeclineVotingRightsRequest(state.NewSession(f.State), "alice"); has {
		t.Fatal("request should be removed after cancel")
	}

	t.Log("--- Failure cancelling non-existent request")
	if err := f.ApplyOp(op.DeclineVotingRights{
		Account: "alice",
		Decline: false,
	}); err == nil {
		t.Fatal("expected no-pending-request failure")
	}

	t.Log("--- Re-decline + advance 30d → request fires, votes cleared")
	if err := f.ApplyOp(op.DeclineVotingRights{
		Account: "alice",
		Decline: true,
	}); err != nil {
		t.Fatalf("re-decline: %v", err)
	}

	// Make alice cast a witness vote (via bob who's a witness once we
	// register him).
	// First make bob a witness.
	if err := f.ApplyOp(op.WitnessUpdate{
		Owner:           "bob",
		URL:             "foo.bar",
		BlockSigningKey: [33]byte{0x02, 0x01},
		Props: op.LegacyChainProperties{
			AccountCreationFee: LegacyHIVE(1_000),
			MaximumBlockSize:   65536,
			HBDInterestRate:    1000,
		},
	}); err != nil {
		t.Fatalf("witness: %v", err)
	}
	if err := f.ApplyOp(op.AccountWitnessVote{
		Account: "alice", Witness: "bob", Approve: true,
	}); err != nil {
		t.Fatalf("alice→bob: %v", err)
	}
	if !f.HasWitnessVote("alice", "bob") {
		t.Fatal("alice→bob vote should exist")
	}

	// Skip 30 days + 1 block.
	f.SkipSeconds(60 * 60 * 24 * 30)
	f.GenerateBlock()

	if !f.Account_("alice").VoteRevoked {
		t.Fatal("alice.VoteRevoked should be true after effective date")
	}
	if f.HasWitnessVote("alice", "bob") {
		t.Fatal("alice→bob vote should be cleared by the effective hook")
	}

	t.Log("--- Failure voting after rights revoked")
	if err := f.ApplyOp(op.AccountWitnessVote{
		Account: "alice", Witness: "bob", Approve: true,
	}); err == nil {
		t.Fatal("expected can_vote=false rejection")
	}
}

// Port of operation_tests.cpp::delegate_vesting_shares_apply (subset).
//
// HF17+: alice delegates VESTS to bob. Reducing the delegation
// schedules the returned VESTS to come back after 5 days.
func TestDelegateVestingSharesApply(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundVesting("alice", 10_000) // 10M VESTS-sat
	f.GenerateBlock()

	preBobReceived := f.Account_("bob").ReceivedVestingShares
	preAliceDelegated := f.Account_("alice").DelegatedVestingShares

	t.Log("--- Initial delegation alice → bob 5M VESTS")
	if err := f.ApplyOp(op.DelegateVestingShares{
		Delegator:     "alice",
		Delegatee:     "bob",
		VestingShares: VESTS(5_000_000),
	}); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if got := f.Account_("alice").DelegatedVestingShares - preAliceDelegated; got != 5_000_000 {
		t.Fatalf("alice DelegatedVestingShares delta: got %d, want 5000000", got)
	}
	if got := f.Account_("bob").ReceivedVestingShares - preBobReceived; got != 5_000_000 {
		t.Fatalf("bob ReceivedVestingShares delta: got %d, want 5000000", got)
	}

	t.Log("--- Reducing the delegation schedules a return")
	if err := f.ApplyOp(op.DelegateVestingShares{
		Delegator:     "alice",
		Delegatee:     "bob",
		VestingShares: VESTS(2_000_000),
	}); err != nil {
		t.Fatalf("reduce delegate: %v", err)
	}
	// bob's received drops immediately by 3M.
	if got := f.Account_("bob").ReceivedVestingShares; got != preBobReceived+2_000_000 {
		t.Fatalf("bob ReceivedVestingShares after reduce: got %d, want %d", got, preBobReceived+2_000_000)
	}
	// alice's DelegatedVestingShares stays high until expiration matures.
	if got := f.Account_("alice").DelegatedVestingShares; got != preAliceDelegated+5_000_000 {
		t.Fatalf("alice DelegatedVestingShares after reduce: got %d, want %d (still locked)",
			got, preAliceDelegated+5_000_000)
	}

	t.Log("--- After 5+ days the locked VESTS return to alice")
	f.SkipSeconds(60*60*24*5 + 60)
	if got := f.Account_("alice").DelegatedVestingShares; got != preAliceDelegated+2_000_000 {
		t.Fatalf("alice DelegatedVestingShares after 5d: got %d, want %d",
			got, preAliceDelegated+2_000_000)
	}
}
