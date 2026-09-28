package testfixture

import (
	"testing"

	"gopherd/internal/chain"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
)

// TestProxyClearedOperationBasic ports
// operation_tests.cpp::proxy_cleared_operation_basic.
//
// hived emits the proxy_cleared_operation virtual op in FOUR cases:
//
//  1. account_witness_proxy with proxy="" and account previously had a
//     proxy (the proxy is being removed).
//  2. account_witness_proxy with a NEW proxy when account previously
//     had a different proxy (the OLD proxy is detached).
//  3. After a decline_voting_rights request's 30-day waiting window
//     elapses, the request is processed and the account's current proxy
//     (if any) is cleared.
//  4. After HF25, governance vote expiration (365 days since the last
//     governance action) clears the account's proxy.
//
// Cases 1 and 2 are covered by accountWitnessProxyEvaluator
// (evaluator/witness.go); case 3 is wired into processDeclineVotingRights
// (chain/decline_voting_rights.go); case 4 is wired into
// removeExpiredGovernanceVotes (chain/governance_votes.go).
//
// This is the C++ smoke test that exercises all four sites in sequence.
func TestProxyClearedOperationBasic(t *testing.T) {
	f := New(t)
	// HF25 activates the governance-vote-expiration sweep (case 4). The
	// decline_voting_rights settlement (case 3) requires HF14+ which is
	// included.
	f.HardforksUpTo(hardfork.HF25)
	f.Actors("alice", "bob", "carol", "dan")
	f.FundVesting("alice", 1_000)
	f.FundVesting("bob", 3_000)
	f.FundVesting("carol", 3_000)
	f.FundVesting("dan", 3_000)
	f.GenerateBlock()

	// --- Sub-block 1: bob → alice. bob had no prior proxy, so NO vop is
	// emitted (oldProxy == "").
	if err := f.ApplyOp(op.AccountWitnessProxy{Account: "bob", Proxy: "alice"}); err != nil {
		t.Fatalf("bob → alice: %v", err)
	}
	if string(f.Account_("bob").Proxy) != "alice" {
		t.Fatalf("bob.Proxy = %q, want alice", f.Account_("bob").Proxy)
	}
	if n := chain.CountVOps(f.Chain.LastBlockVOps(), op.TagProxyCleared); n != 0 {
		t.Fatalf("setting first proxy should NOT emit proxy_cleared vop; got %d", n)
	}

	// --- Sub-block 2: bob clears proxy (proxy=""). bob.Proxy was alice;
	// vop emitted {account=bob, proxy=alice}.
	if err := f.ApplyOp(op.AccountWitnessProxy{Account: "bob", Proxy: ""}); err != nil {
		t.Fatalf("bob → (clear): %v", err)
	}
	if string(f.Account_("bob").Proxy) != "" {
		t.Fatalf("bob.Proxy = %q, want empty", f.Account_("bob").Proxy)
	}
	if got := findProxyClearedVOp(f.Chain.LastBlockVOps()); got == nil {
		t.Fatalf("clearing proxy: expected one proxy_cleared vop; got %+v", f.Chain.LastBlockVOps())
	} else if string(got.Account) != "bob" || string(got.Proxy) != "alice" {
		t.Fatalf("clearing proxy: vop = {account=%q, proxy=%q}, want {bob, alice}", got.Account, got.Proxy)
	}

	// --- Sub-block 3: bob sets proxy to carol AND immediately submits
	// decline_voting_rights in the SAME tx. After the 30-day window
	// elapses, the decline_voting_rights settlement fires and clears
	// bob's proxy (carol), emitting proxy_cleared{bob, carol}.
	f.PushOps(
		op.AccountWitnessProxy{Account: "bob", Proxy: "carol"},
		op.DeclineVotingRights{Account: "bob", Decline: true},
	)
	f.GenerateBlock()
	if string(f.Account_("bob").Proxy) != "carol" {
		t.Fatalf("after proxy+decline tx: bob.Proxy = %q, want carol", f.Account_("bob").Proxy)
	}
	// Advance the chain past the 30-day owner-auth recovery window
	// (HIVE_OWNER_AUTH_RECOVERY_PERIOD = 60 * 60 * 24 * 30 sec).
	const ownerAuthRecoveryPeriodSec uint32 = 60 * 60 * 24 * 30
	f.SkipSeconds(ownerAuthRecoveryPeriodSec + BlockInterval)
	if string(f.Account_("bob").Proxy) != "" {
		t.Fatalf("after 30d: bob.Proxy = %q, want empty (decline_voting_rights settled)",
			f.Account_("bob").Proxy)
	}
	if !f.Account_("bob").VoteRevoked {
		t.Fatalf("after 30d: bob.VoteRevoked = false, want true")
	}
	if got := findProxyClearedVOp(f.Chain.LastBlockVOps()); got == nil {
		t.Fatalf("decline settlement: expected one proxy_cleared vop; got %+v", f.Chain.LastBlockVOps())
	} else if string(got.Account) != "bob" || string(got.Proxy) != "carol" {
		t.Fatalf("decline settlement: vop = {account=%q, proxy=%q}, want {bob, carol}",
			got.Account, got.Proxy)
	}

	// --- Sub-block 4: carol sets proxy=dan. After
	// HIVE_GOVERNANCE_VOTE_EXPIRATION_PERIOD (365 days), the governance
	// vote expiration sweep clears carol's proxy, emitting
	// proxy_cleared{carol, dan}.
	if err := f.ApplyOp(op.AccountWitnessProxy{Account: "carol", Proxy: "dan"}); err != nil {
		t.Fatalf("carol → dan: %v", err)
	}
	if string(f.Account_("carol").Proxy) != "dan" {
		t.Fatalf("carol.Proxy = %q, want dan", f.Account_("carol").Proxy)
	}
	// Advance the chain past the 365-day governance expiration window.
	const governanceVoteExpirationPeriodSec uint32 = 365 * 24 * 60 * 60
	f.SkipSeconds(governanceVoteExpirationPeriodSec + BlockInterval)
	if string(f.Account_("carol").Proxy) != "" {
		t.Fatalf("after 365d: carol.Proxy = %q, want empty (governance vote expiration cleared it)",
			f.Account_("carol").Proxy)
	}
	if got := findProxyClearedVOp(f.Chain.LastBlockVOps()); got == nil {
		t.Fatalf("governance expiration: expected one proxy_cleared vop; got %+v", f.Chain.LastBlockVOps())
	} else if string(got.Account) != "carol" || string(got.Proxy) != "dan" {
		t.Fatalf("governance expiration: vop = {account=%q, proxy=%q}, want {carol, dan}",
			got.Account, got.Proxy)
	}
}

// findProxyClearedVOp returns the first proxy_cleared op in the block's
// virtual-op stream, or nil. Other vops (producer_reward, hardfork,
// fill_recurrent_transfer, …) are skipped — this asserts proxy_cleared
// semantics only.
func findProxyClearedVOp(vops []op.Operation) *op.ProxyCleared {
	if v := chain.FindVOp(vops, op.TagProxyCleared); v != nil {
		pc := v.(op.ProxyCleared)
		return &pc
	}
	return nil
}
