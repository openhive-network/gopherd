package testfixture

import (
	"strings"
	"testing"

	"hived-go/internal/protocol/op"
)

// TestAccountWitnessProxyTooLong ports
// operation_tests.cpp::account_witness_proxy_too_long.
//
// We build a chain henry → greg → dave → sam and then try to attach
// alice on top (alice → henry → greg → dave → sam → 5 links from
// account 'alice'). Per hive_evaluator.cpp:218-232 the proxy_chain set
// is seeded with {alice, henry} (size 2) and must satisfy
// proxy_chain.size() <= HIVE_MAX_PROXY_RECURSION_DEPTH (4) after walking
// every existing proxy step. Walking henry → greg → dave → sam pushes
// the set to size 5, which the chain rejects.
//
// The C++ test then verifies sam's proxied_vsf_votes slots, attaches
// sam → bob → alice to push the chain to 5 entries, and confirms
// henry's vest stops propagating past bob. We focus on the assertion
// gate; the propagation invariants are covered by other proxy tests.
func TestAccountWitnessProxyTooLong(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob", "sam", "dave", "greg", "henry")
	f.FundVesting("alice", 1_000)
	f.FundVesting("bob", 3_000)
	f.FundVesting("sam", 5_000)
	f.FundVesting("dave", 7_000)
	f.FundVesting("greg", 9_000)
	f.FundVesting("henry", 11_000)
	f.GenerateBlock()

	// Build chain henry → greg → dave → sam (3 proxy hops above sam).
	if err := f.ApplyOp(op.AccountWitnessProxy{Account: "dave", Proxy: "sam"}); err != nil {
		t.Fatalf("dave→sam: %v", err)
	}
	if err := f.ApplyOp(op.AccountWitnessProxy{Account: "greg", Proxy: "dave"}); err != nil {
		t.Fatalf("greg→dave: %v", err)
	}
	if err := f.ApplyOp(op.AccountWitnessProxy{Account: "henry", Proxy: "greg"}); err != nil {
		t.Fatalf("henry→greg: %v", err)
	}

	// Now alice tries to attach on top: alice → henry would build a
	// chain of length 5 from alice. C++ asserts proxy_chain.size() <= 4.
	err := f.ApplyOp(op.AccountWitnessProxy{Account: "alice", Proxy: "henry"})
	if err == nil {
		t.Fatal("expected proxy_chain length assert to reject alice → henry (chain of 5)")
	}
	if !strings.Contains(err.Error(), "HIVE_MAX_PROXY_RECURSION_DEPTH") &&
		!strings.Contains(err.Error(), "proxy_chain") {
		t.Fatalf("error should mention proxy chain depth: %v", err)
	}

	// Attaching from the OTHER end works: sam → bob.
	if err := f.ApplyOp(op.AccountWitnessProxy{Account: "sam", Proxy: "bob"}); err != nil {
		t.Fatalf("sam→bob: %v", err)
	}
	// bob → alice still works (chain is henry → greg → dave → sam →
	// bob → alice, 5 hops). C++'s comment notes that even when alice
	// is the top, henry's stake won't propagate past bob.
	if err := f.ApplyOp(op.AccountWitnessProxy{Account: "bob", Proxy: "alice"}); err != nil {
		t.Fatalf("bob→alice: %v", err)
	}

	// At this point alice's proxied_vsf_votes should contain
	// bob/sam/dave/greg's vest (slots 0..3); henry's vest should NOT
	// appear (beyond depth limit).
	alice := f.Account_("alice")
	if alice.ProxiedVSFVotes[0] != f.Account_("bob").VestingShares {
		t.Errorf("alice.ProxiedVSFVotes[0]: got %d, want bob.VestingShares=%d",
			alice.ProxiedVSFVotes[0], f.Account_("bob").VestingShares)
	}
	if alice.ProxiedVSFVotes[1] != f.Account_("sam").VestingShares {
		t.Errorf("alice.ProxiedVSFVotes[1]: got %d, want sam.VestingShares=%d",
			alice.ProxiedVSFVotes[1], f.Account_("sam").VestingShares)
	}
}
