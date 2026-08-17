package chain

import (
	"testing"

	"gopherd/internal/crypto"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
)

// TestWitnessBlockApproveRequiresWitnessCategory pins the bug that let a flood
// of witness_block_approve into our mempool and produced a block the network
// rejected (108912883).
//
// The operation declares NO account-tier authority — correctly, matching
// hived. The danger is reading that as "no authority required": hived
// authorises it through the separate get_required_witness_signatures category
// (hive_operations.hpp:1047 -> transaction_util.cpp:169), so a transaction
// carrying one must be checked against the WITNESS's signing key.
//
// If AuthorityWitness is ever dropped, RequiredAuthorities reports an empty set
// for such a transaction and VerifyAuthorityWithKeys returns early — accepting
// ANY signature, which is exactly the defect.
func TestWitnessBlockApproveRequiresWitnessCategory(t *testing.T) {
	signed := &tx.SignedTransaction{Transaction: tx.Transaction{
		Operations: op.Ops(op.WitnessBlockApprove{Witness: "gtg"}),
	}}

	req := RequiredAuthorities(signed)

	if len(req[AuthorityWitness]) != 1 {
		t.Fatalf("witness_block_approve must require the WITNESS category, got %v", req[AuthorityWitness])
	}
	if _, ok := req[AuthorityWitness]["gtg"]; !ok {
		t.Fatalf("expected witness %q in the witness category, got %v", "gtg", req[AuthorityWitness])
	}
	// It must NOT be smuggled into an account tier — those are satisfiable by
	// the account's own keys, which is not the rule for this operation.
	for _, lvl := range []AuthorityLevel{AuthorityActive, AuthorityOwner, AuthorityPosting} {
		if len(req[lvl]) != 0 {
			t.Fatalf("witness_block_approve must declare no account-tier authority, level %v has %v", lvl, req[lvl])
		}
	}
}

// TestSignedByKeyMarksConsumed pins that satisfying the witness category marks
// the signature USED.
//
// Without this, the pre-HF28 remove_unused_signatures assert
// (transaction_util.cpp:111) sees an unconsumed key and rejects a
// correctly-signed witness_block_approve as carrying an irrelevant signature —
// turning a fix into a different rejection.
func TestSignedByKeyMarksConsumed(t *testing.T) {
	var k crypto.PublicKey
	k[0] = 0x02
	k[1] = 0x42

	s := newSignState([]crypto.PublicKey{k}, nil)

	var absent crypto.PublicKey
	absent[0] = 0x03
	if s.signedByKey(absent) {
		t.Fatal("signedByKey must be false for a key that did not sign")
	}
	if s.usedKeys[absent] {
		t.Fatal("a key that did not sign must not be marked used")
	}

	if !s.signedByKey(k) {
		t.Fatal("signedByKey must be true for a key that signed")
	}
	if !s.usedKeys[k] {
		t.Fatal("satisfying the witness category must CONSUME the signature, else it trips tx_irrelevant_sig")
	}
}

// TestWitnessBlockApproveNeverAdmitted pins the exclusion that keeps our blocks
// matching the network's.
//
// hived's evaluator is a hard assert, not a validation rule
// (hive_evaluator.cpp:615): "This operation may not be included in a block". It
// is a FAST-CONFIRMATION message routed through
// database::process_fast_confirm_transaction, never through push_transaction —
// which is why it is ~92% of gossiped traffic and appears in ZERO real blocks.
//
// Being correctly signed is NOT a reason to admit it. I removed the exclusion
// once on exactly that reasoning, after implementing the witness-signature
// authority category, and block assembly regressed instantly from only-ours=0
// to ours=379 against theirs=32.
func TestWitnessBlockApproveNeverAdmitted(t *testing.T) {
	signed := &tx.SignedTransaction{Transaction: tx.Transaction{
		Operations: op.Ops(op.WitnessBlockApprove{Witness: "gtg"}),
	}}
	var found bool
	for i := range signed.Operations {
		if signed.Operations[i].Tag == op.TagWitnessBlockApprove {
			found = true
		}
	}
	if !found {
		t.Fatal("test fixture must carry a witness_block_approve operation")
	}
	// The admission path must refuse it by TAG, independently of whether its
	// signature verifies — verification passing is precisely the trap.
	if !OpExcludedFromBlocks(op.TagWitnessBlockApprove) {
		t.Fatal("witness_block_approve must be excluded from block assembly")
	}
	// Ordinary operations must NOT be swept up by the exclusion.
	for _, tag := range []op.Tag{op.TagVote, op.TagComment, op.TagTransfer, op.TagCustomJSON} {
		if OpExcludedFromBlocks(tag) {
			t.Fatalf("%s must remain admissible", tag.Name())
		}
	}
}
