package op

import "testing"

// TestAutodetectKeysSimpleSingleActive ports a slice of
// transaction_util_tests.cpp::autodetect_keys_simple. A single
// transfer from alice → bob requires alice's active authority only.
func TestAutodetectKeysSimpleSingleActive(t *testing.T) {
	tx := []Operation{
		Transfer{From: "alice", To: "bob", Amount: hive(10), Memo: "hi"},
	}
	auths := CollectRequiredAuthorities(tx)
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d: %+v", len(auths), auths)
	}
	if auths[0].Account != "alice" || auths[0].Level != AuthActive {
		t.Errorf("expected alice/active, got %+v", auths[0])
	}
}

// TestAutodetectKeysSingleActiveManyOps verifies that two transfers
// from the same sender produce ONE auth requirement (deduped).
func TestAutodetectKeysSingleActiveManyOps(t *testing.T) {
	tx := []Operation{
		Transfer{From: "alice", To: "bob", Amount: hive(10)},
		Transfer{From: "alice", To: "carol", Amount: hive(20)},
	}
	auths := CollectRequiredAuthorities(tx)
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth (deduped), got %d: %+v", len(auths), auths)
	}
}

// TestAutodetectKeysManyActive verifies that two transfers from
// different senders produce two auth requirements.
func TestAutodetectKeysManyActive(t *testing.T) {
	tx := []Operation{
		Transfer{From: "alice", To: "bob", Amount: hive(10)},
		Transfer{From: "carol", To: "dave", Amount: hive(20)},
	}
	auths := CollectRequiredAuthorities(tx)
	if len(auths) != 2 {
		t.Fatalf("expected 2 auths, got %d: %+v", len(auths), auths)
	}
}

// TestAutodetectKeysActiveAndOwner verifies that mixing transfer (active)
// with decline_voting_rights (owner) for the same account produces TWO
// distinct requirements.
func TestAutodetectKeysActiveAndOwner(t *testing.T) {
	tx := []Operation{
		Transfer{From: "alice", To: "bob", Amount: hive(10)},
		DeclineVotingRights{Account: "alice", Decline: true},
	}
	auths := CollectRequiredAuthorities(tx)
	if len(auths) != 2 {
		t.Fatalf("expected 2 auths (alice/active + alice/owner), got %d: %+v", len(auths), auths)
	}
	hasActive, hasOwner := false, false
	for _, a := range auths {
		if a.Account == "alice" && a.Level == AuthActive {
			hasActive = true
		}
		if a.Account == "alice" && a.Level == AuthOwner {
			hasOwner = true
		}
	}
	if !hasActive || !hasOwner {
		t.Errorf("expected both alice/active and alice/owner; got %+v", auths)
	}
}

// TestAutodetectKeysSinglePosting ports the single-posting branch:
// comment_operation requires posting authority.
func TestAutodetectKeysSinglePosting(t *testing.T) {
	tx := []Operation{
		Comment{Author: "alice", Permlink: "p", Body: "hi"},
	}
	auths := CollectRequiredAuthorities(tx)
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if auths[0].Account != "alice" || auths[0].Level != AuthPosting {
		t.Errorf("expected alice/posting, got %+v", auths[0])
	}
}

// TestAutodetectKeysVoteIsPosting verifies vote_operation requires posting.
func TestAutodetectKeysVoteIsPosting(t *testing.T) {
	tx := []Operation{
		Vote{Voter: "alice", Author: "bob", Permlink: "p", Weight: 10000},
	}
	auths := CollectRequiredAuthorities(tx)
	if len(auths) != 1 || auths[0].Level != AuthPosting {
		t.Fatalf("expected alice/posting, got %+v", auths)
	}
}

// TestAutodetectKeysWitnessVoteIsActive verifies account_witness_vote
// is active level (not posting), unlike comment voting.
func TestAutodetectKeysWitnessVoteIsActive(t *testing.T) {
	tx := []Operation{
		AccountWitnessVote{Account: "alice", Witness: "bob", Approve: true},
	}
	auths := CollectRequiredAuthorities(tx)
	if len(auths) != 1 || auths[0].Level != AuthActive {
		t.Fatalf("expected alice/active, got %+v", auths)
	}
}

// TestAutodetectKeysFeedPublishIsActive verifies feed_publish from a
// witness requires that witness's active authority.
func TestAutodetectKeysFeedPublishIsActive(t *testing.T) {
	tx := []Operation{
		FeedPublish{Publisher: "alice"},
	}
	auths := CollectRequiredAuthorities(tx)
	if len(auths) != 1 || auths[0].Account != "alice" || auths[0].Level != AuthActive {
		t.Fatalf("expected alice/active, got %+v", auths)
	}
}

// TestAutodetectKeysWitnessSetPropertiesNoRequirement verifies that
// witness_set_properties — which self-attests via the "key" prop —
// does NOT add a normal authority requirement.
func TestAutodetectKeysWitnessSetPropertiesNoRequirement(t *testing.T) {
	tx := []Operation{
		WitnessSetProperties{Owner: "alice"},
	}
	auths := CollectRequiredAuthorities(tx)
	if len(auths) != 0 {
		t.Fatalf("expected 0 auths (self-attesting), got %+v", auths)
	}
}

// TestAutodetectKeysProposalActions verify proposal ops require active.
func TestAutodetectKeysProposalActions(t *testing.T) {
	cases := []struct {
		op      Operation
		account string
	}{
		{CreateProposal{Creator: "alice"}, "alice"},
		{UpdateProposal{Creator: "alice"}, "alice"},
		{UpdateProposalVotes{Voter: "alice"}, "alice"},
		{RemoveProposal{ProposalOwner: "alice"}, "alice"},
	}
	for _, c := range cases {
		auths := CollectRequiredAuthorities([]Operation{c.op})
		if len(auths) != 1 {
			t.Errorf("op %T: expected 1 auth, got %+v", c.op, auths)
			continue
		}
		if string(auths[0].Account) != c.account || auths[0].Level != AuthActive {
			t.Errorf("op %T: got %+v, want %s/active", c.op, auths[0], c.account)
		}
	}
}
