package testfixture

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
)

// TestDelegateRCFailureInBlockIsANoOp pins hive_evaluator.cpp:355-370 for
// the "rc" custom_json interpreter: an exception thrown while applying a
// block (not in control) is swallowed and the operation's writes are rolled
// back by the interpreter's undo session. Before this, a delegate_rc naming a
// missing delegatee, or over-delegating, failed the transaction and with it
// a block hived had accepted (review item A15).
func TestDelegateRCFailureInBlockIsANoOp(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF26)
	f.SetMedianAccountCreationFee(0)
	f.Actors("alice", "bob", "carol")
	f.FundVesting("alice", 1_000)
	f.GenerateBlock()

	// A delegatee that does not exist: fatal at the head ...
	bad := rcDelegationCustomOp("alice", []string{"bob", "nobody"}, 1_000)
	if err := f.ApplyOp(bad); err == nil {
		t.Fatal("delegate_rc to a missing account accepted while in control")
	}
	// ... a no-op inside a block, with nothing left behind for bob.
	if err := f.ApplyOpNotInControl(bad); err != nil {
		t.Fatalf("delegate_rc to a missing account failed the block (hived swallows it): %v", err)
	}
	if _, ok := rcDelegation(f, "alice", "bob"); ok {
		t.Fatal("alice→bob delegation survived the rolled-back op")
	}
	if got := f.Account_("alice").DelegatedRCOut; got != 0 {
		t.Fatalf("alice.DelegatedRCOut = %d after a rolled-back op, want 0", got)
	}

	// A failure AFTER state was written: two delegations of 2^62 each
	// overflow delta_total on the second delegatee, after bob's row and
	// mana were already stored. The savepoint must take bob's row with it.
	// (Over-delegation itself is deliberately not re-checked in a block —
	// see the step-7 comment in applyDelegateRC.)
	overflow := rcDelegationCustomOp("alice", []string{"bob", "carol"}, 1<<62)
	if err := f.ApplyOp(overflow); err == nil {
		t.Fatal("delta_total overflow accepted while in control")
	}
	if err := f.ApplyOpNotInControl(overflow); err != nil {
		t.Fatalf("delta_total overflow failed the block: %v", err)
	}
	if _, ok := rcDelegation(f, "alice", "bob"); ok {
		t.Fatal("alice→bob row written before the failure survived the rollback")
	}
	if got := f.Account_("bob").DelegatedRCIn; got != 0 {
		t.Fatalf("bob.DelegatedRCIn = %d after a rolled-back op, want 0", got)
	}

	// And a valid one in a block still applies, so the savepoint path is
	// not simply discarding everything.
	good := rcDelegationCustomOp("alice", []string{"bob"}, 1_000)
	if err := f.ApplyOpNotInControl(good); err != nil {
		t.Fatalf("valid delegate_rc in a block: %v", err)
	}
	if d, ok := rcDelegation(f, "alice", "bob"); !ok || int64(d.DelegatedRC) != 1_000 {
		t.Fatalf("alice→bob after a valid in-block delegate_rc: %+v %v", d, ok)
	}
}
