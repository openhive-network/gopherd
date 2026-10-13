package testfixture

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
)

// TestDelegateRCBatchedPayload pins generic_custom_operation_interpreter's list
// form: one "rc" custom_json carrying several operations, each a
// ["delegate_rc", {...}] pair. ecency delegates this way (e.g. tx ce411e9b at
// block 81,787,894). gopherd used to read only a single operation, so every
// batched payload was a silent no-op: ecency ended up with 478 delegations
// where hived has 93, and 165 of 1,095 recently active accounts had a
// different max_rc than mainnet.
func TestDelegateRCBatchedPayload(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF26)
	f.SetMedianAccountCreationFee(0)
	f.Actors("alice", "bob", "carol", "dave")
	f.FundVesting("alice", 1_000)
	f.GenerateBlock()

	batch := func(json string) op.CustomJSON {
		return op.CustomJSON{RequiredPostingAuths: []string{"alice"}, ID: "rc", JSON: json}
	}

	set := batch(`[["delegate_rc",{"from":"alice","delegatees":["bob"],"max_rc":1000}],` +
		`["delegate_rc",{"from":"alice","delegatees":["carol"],"max_rc":2000}],` +
		`{"type":"delegate_rc_operation","value":{"from":"alice","delegatees":["dave"],"max_rc":"3000"}}]`)
	if err := f.ApplyOp(set); err != nil {
		t.Fatalf("batched delegate_rc refused: %v", err)
	}
	for to, want := range map[string]uint64{"bob": 1000, "carol": 2000, "dave": 3000} {
		d, ok := rcDelegation(f, "alice", to)
		if !ok || d.DelegatedRC != want {
			t.Fatalf("alice→%s = %v (present %v), want %d", to, d, ok, want)
		}
	}
	if got := f.Account_("alice").DelegatedRCOut; got != 6000 {
		t.Fatalf("alice.DelegatedRCOut = %d, want 6000", got)
	}

	// Removing in a batch, the shape ecency uses to clean up.
	unset := batch(`[["delegate_rc",{"from":"alice","delegatees":["bob"],"max_rc":0}],` +
		`["delegate_rc",{"from":"alice","delegatees":["carol"],"max_rc":0}]]`)
	if err := f.ApplyOpNotInControl(unset); err != nil {
		t.Fatalf("batched removal in a block: %v", err)
	}
	for _, to := range []string{"bob", "carol"} {
		if _, ok := rcDelegation(f, "alice", to); ok {
			t.Fatalf("alice→%s survived a batched removal", to)
		}
	}
	if got := f.Account_("alice").DelegatedRCOut; got != 3000 {
		t.Fatalf("alice.DelegatedRCOut = %d after removing bob and carol, want 3000", got)
	}

	// All or nothing: the second operation names a missing delegatee, so the
	// first must not stick either — in a block, and at the head.
	partial := batch(`[["delegate_rc",{"from":"alice","delegatees":["bob"],"max_rc":500}],` +
		`["delegate_rc",{"from":"alice","delegatees":["nobody"],"max_rc":500}]]`)
	if err := f.ApplyOp(partial); err == nil {
		t.Fatal("batch with a missing delegatee accepted while in control")
	}
	if err := f.ApplyOpNotInControl(partial); err != nil {
		t.Fatalf("batch with a missing delegatee failed the block (hived swallows it): %v", err)
	}
	if _, ok := rcDelegation(f, "alice", "bob"); ok {
		t.Fatal("first operation of a failed batch survived the rollback")
	}

	// Authorities: every operation's from must be among the posting auths and
	// vice versa, and no active authority may be named.
	foreign := batch(`[["delegate_rc",{"from":"alice","delegatees":["bob"],"max_rc":500}],` +
		`["delegate_rc",{"from":"carol","delegatees":["bob"],"max_rc":500}]]`)
	if err := f.ApplyOp(foreign); err == nil {
		t.Fatal("batch delegating from an account that did not sign was accepted")
	}
	active := batch(`["delegate_rc",{"from":"alice","delegatees":["bob"],"max_rc":500}]`)
	active.RequiredAuths = []string{"alice"}
	if err := f.ApplyOp(active); err == nil {
		t.Fatal("delegate_rc under an active authority was accepted")
	}
}
