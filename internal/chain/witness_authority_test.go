package chain

import (
	"testing"

	"hived-go/internal/crypto"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
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
