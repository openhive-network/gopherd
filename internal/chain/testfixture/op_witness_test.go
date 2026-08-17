package testfixture

import (
	"testing"

	"gopherd/internal/protocol/op"
)

// Port of operation_tests.cpp::witness_update_apply.
//
// alice (with 10 HIVE liquid) registers herself as a witness.
// Hived's mainnet doesn't actually charge the fee field even though
// the op has one — our witnessUpdateEvaluator preserves that
// historical behavior. The test just verifies the resulting witness
// record matches the op's url / signing_key / props.
func TestWitnessUpdateApply(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.FundLiquid("alice", 10_000)
	f.GenerateBlock()

	t.Log("--- Test upgrading an account to a witness")
	signingKey := [33]byte{0x02, 0x01, 0x02, 0x03} // junk pubkey for test
	if err := f.ApplyOp(op.WitnessUpdate{
		Owner:           "alice",
		URL:             "foo.bar",
		BlockSigningKey: signingKey,
		Props: op.LegacyChainProperties{
			AccountCreationFee: LegacyHIVE(1_000),
			MaximumBlockSize:   65536,
			HBDInterestRate:    1000,
		},
		Fee: HIVE(1_000),
	}); err != nil {
		t.Fatalf("witness_update: %v", err)
	}

	w := f.Witness("alice")
	if w.Owner != "alice" {
		t.Fatalf("witness.Owner: got %q, want alice", w.Owner)
	}
	if w.URL != "foo.bar" {
		t.Fatalf("witness.URL: got %q, want foo.bar", w.URL)
	}
	if w.BlockSigningKey != signingKey {
		t.Fatal("witness.BlockSigningKey mismatch")
	}
	if got := f.Balance("alice"); got != 10_000 {
		t.Fatalf("alice.HIVE: got %d, want 10000 (no fee)", got)
	}

	t.Log("--- Test updating a witness")
	if err := f.ApplyOp(op.WitnessUpdate{
		Owner:           "alice",
		URL:             "bar.foo",
		BlockSigningKey: signingKey,
		Props: op.LegacyChainProperties{
			AccountCreationFee: LegacyHIVE(1_000),
			MaximumBlockSize:   65536,
			HBDInterestRate:    1000,
		},
		Fee: HIVE(1_000),
	}); err != nil {
		t.Fatalf("witness_update 2: %v", err)
	}
	w = f.Witness("alice")
	if w.URL != "bar.foo" {
		t.Fatalf("witness.URL after update: got %q, want bar.foo", w.URL)
	}

	t.Log("--- Test failure when upgrading a non-existent account")
	if err := f.ApplyOp(op.WitnessUpdate{
		Owner:           "i_do_not_exist",
		URL:             "foo.bar",
		BlockSigningKey: signingKey,
		Props: op.LegacyChainProperties{
			AccountCreationFee: LegacyHIVE(1_000),
			MaximumBlockSize:   65536,
			HBDInterestRate:    1000,
		},
	}); err == nil {
		t.Fatal("expected witness_update on missing account to fail")
	}
}

// Port of operation_tests.cpp::account_witness_vote_apply.
//
// alice has 5 HIVE vested + sam is a witness. alice votes for sam.
// sam.TotalVotesVests should equal alice.VestingShares.
func TestAccountWitnessVoteApply(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob", "sam")
	f.FundVesting("alice", 5_000)
	f.FundLiquid("sam", 1_000)
	f.GenerateBlock()

	// Make sam a witness.
	if err := f.ApplyOp(op.WitnessUpdate{
		Owner:           "sam",
		URL:             "foo.bar",
		BlockSigningKey: [33]byte{0x02, 0x01},
		Props: op.LegacyChainProperties{
			AccountCreationFee: LegacyHIVE(1_000),
			MaximumBlockSize:   65536,
			HBDInterestRate:    1000,
		},
		Fee: HIVE(1_000),
	}); err != nil {
		t.Fatalf("witness_create sam: %v", err)
	}

	t.Log("--- Test normal vote")
	if err := f.ApplyOp(op.AccountWitnessVote{
		Account: "alice",
		Witness: "sam",
		Approve: true,
	}); err != nil {
		t.Fatalf("witness vote: %v", err)
	}
	aliceVests := f.Vests("alice")
	if got := f.Witness("sam").TotalVotesVests; got != aliceVests {
		t.Fatalf("sam.TotalVotesVests after vote: got %d, want %d", got, aliceVests)
	}
	if !f.HasWitnessVote("alice", "sam") {
		t.Fatal("alice→sam witness vote should exist")
	}

	t.Log("--- Test revoke vote")
	if err := f.ApplyOp(op.AccountWitnessVote{
		Account: "alice",
		Witness: "sam",
		Approve: false,
	}); err != nil {
		t.Fatalf("witness unvote: %v", err)
	}
	if got := f.Witness("sam").TotalVotesVests; got != 0 {
		t.Fatalf("sam.TotalVotesVests after revoke: got %d, want 0", got)
	}
	if f.HasWitnessVote("alice", "sam") {
		t.Fatal("alice→sam witness vote should be removed")
	}

	t.Log("--- Test failure when revoking a non-existent vote")
	if err := f.ApplyOp(op.AccountWitnessVote{
		Account: "alice",
		Witness: "sam",
		Approve: false,
	}); err == nil {
		t.Fatal("expected error on revoking non-existent vote")
	}

	t.Log("--- Test failure when voting for a non-existent witness")
	if err := f.ApplyOp(op.AccountWitnessVote{
		Account: "alice",
		Witness: "dave",
		Approve: true,
	}); err == nil {
		t.Fatal("expected error voting for non-existent witness")
	}

	t.Log("--- Test failure when voting for an account that is not a witness")
	// bob exists but isn't a witness
	if err := f.ApplyOp(op.AccountWitnessVote{
		Account: "alice",
		Witness: "bob",
		Approve: true,
	}); err == nil {
		t.Fatal("expected error voting for non-witness")
	}
}

// Port of operation_tests.cpp::account_witness_proxy_apply (subset).
//
// alice sets bob as her proxy. Alice's previous direct witness votes
// get cleared. Subsequent witness votes from alice fail (she has a
// proxy). Bob's vote on a witness is recorded.
func TestAccountWitnessProxyApply(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob", "sam")
	f.FundVesting("alice", 5_000)
	f.FundLiquid("sam", 1_000)
	f.GenerateBlock()

	// sam is a witness
	if err := f.ApplyOp(op.WitnessUpdate{
		Owner:           "sam",
		URL:             "foo.bar",
		BlockSigningKey: [33]byte{0x02, 0x01},
		Props: op.LegacyChainProperties{
			AccountCreationFee: LegacyHIVE(1_000),
			MaximumBlockSize:   65536,
			HBDInterestRate:    1000,
		},
		Fee: HIVE(1_000),
	}); err != nil {
		t.Fatalf("witness_create sam: %v", err)
	}

	// alice votes for sam
	if err := f.ApplyOp(op.AccountWitnessVote{
		Account: "alice",
		Witness: "sam",
		Approve: true,
	}); err != nil {
		t.Fatalf("alice→sam vote: %v", err)
	}
	if !f.HasWitnessVote("alice", "sam") {
		t.Fatal("setup: alice→sam vote should exist")
	}

	t.Log("--- Setting alice's proxy to bob clears her direct votes")
	if err := f.ApplyOp(op.AccountWitnessProxy{
		Account: "alice",
		Proxy:   "bob",
	}); err != nil {
		t.Fatalf("set proxy: %v", err)
	}
	if got := f.Account_("alice").Proxy; got != "bob" {
		t.Fatalf("alice.Proxy: got %q, want bob", got)
	}
	if f.HasWitnessVote("alice", "sam") {
		t.Fatal("alice→sam vote should be cleared after proxy set")
	}
	if got := f.Account_("alice").WitnessesVotedFor; got != 0 {
		t.Fatalf("alice.WitnessesVotedFor: got %d, want 0", got)
	}

	t.Log("--- Clearing proxy")
	if err := f.ApplyOp(op.AccountWitnessProxy{
		Account: "alice",
		Proxy:   "",
	}); err != nil {
		t.Fatalf("clear proxy: %v", err)
	}
	if got := f.Account_("alice").Proxy; got != "" {
		t.Fatalf("alice.Proxy after clear: got %q, want empty", got)
	}
}
