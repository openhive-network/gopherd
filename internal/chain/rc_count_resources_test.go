package chain

import (
	"testing"

	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
	"gopherd/internal/state/objects"
)

// TestCountResourcesSkippedSignatures: a transaction decoded the replay way,
// signatures skipped, is charged the same execution time as one decoded with
// its signatures kept, verify_authority_time per signature included.
func TestCountResourcesSkippedSignatures(t *testing.T) {
	st := tx.SignedTransaction{
		Transaction: tx.Transaction{
			RefBlockNum: 7, RefBlockPrefix: 1, Expiration: 3600,
			Operations: op.Ops(op.Vote{Voter: "alice", Author: "bob", Permlink: "post", Weight: 10000}),
		},
		Signatures: make([][tx.SignatureSize]byte, 2),
	}
	wire := tx.AppendSignedTransaction(nil, &st)
	full, err := tx.ReadSignedTransaction(fcraw.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	skipped, err := tx.ReadSignedTransactionSkipSigs(fcraw.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	if n := skipped.SignedTx.NumSignatures(); n != 2 {
		t.Fatalf("skipped decode counts %d signatures, want 2", n)
	}
	a := CountResourcesForTx(&full.SignedTx, int64(len(wire)), 0)
	b := CountResourcesForTx(&skipped.SignedTx, int64(len(wire)), 0)
	if a != b {
		t.Fatalf("usage with signatures %v, skipped %v", a, b)
	}
	want := execTimeNS[op.TagVote] + transactionExecTimeNS + 2*verifyAuthorityTimeNS
	if got := b[objects.RCExecutionTime]; got != want {
		t.Fatalf("execution time %d, want %d", got, want)
	}
}
