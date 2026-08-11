package chain

import (
	"testing"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/network"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
)

// TestBlockTransactionBodiesRoundTrip pins the property the admission audit
// depends on: the bytes handed back for each transaction must be the VERBATIM
// wire bytes, re-readable as the same transaction.
//
// If these slices were off by even one byte the audit would report authority
// failures for every transaction and "prove" a filter bug that does not exist —
// the measurement would be worse than none, because it looks decisive.
func TestBlockTransactionBodiesRoundTrip(t *testing.T) {
	raw := buildTestBlockWithTxs(t)

	bodies, err := BlockTransactionBodies(raw)
	if err != nil {
		t.Fatalf("BlockTransactionBodies: %v", err)
	}
	ids := BlockTransactionIDs(raw)
	if len(bodies) != len(ids) {
		t.Fatalf("bodies=%d ids=%d — audit would mis-attribute refusals", len(bodies), len(ids))
	}
	if len(bodies) == 0 {
		t.Fatal("no transactions extracted; the audit would silently measure nothing")
	}

	for i, body := range bodies {
		r := fcraw.NewReader(body)
		res, err := tx.ReadSignedTransaction(r)
		if err != nil {
			t.Fatalf("tx %d: body does not re-read as a transaction: %v", i, err)
		}
		if !r.AtEnd() {
			t.Fatalf("tx %d: %d trailing bytes — slice boundary is wrong", i, r.Remaining())
		}
		// The id computed from the extracted body must match the id computed
		// from the block, i.e. we sliced the same transaction the block holds.
		got := tx.IDFromDigest(tx.DigestFromBytes(body[res.UnsignedStart:res.UnsignedEnd]))
		if got != ids[i] {
			t.Fatalf("tx %d: id mismatch, extracted body is not the block's transaction", i)
		}
	}
}

// buildTestBlockWithTxs produces a serialized block carrying several
// transactions of DIFFERENT sizes, so a boundary bug cannot hide behind
// uniform-length entries.
func buildTestBlockWithTxs(t *testing.T) []byte {
	t.Helper()
	mk := func(id uint16, data []byte) tx.SignedTransaction {
		return tx.SignedTransaction{Transaction: tx.Transaction{
			Operations: op.Ops(op.Custom{ID: id, RequiredAuths: []string{"initminer"}, Data: data}),
			Expiration: network.GenesisTimeUnix + 60,
		}}
	}
	txs := []tx.SignedTransaction{
		mk(1, []byte{0}),
		mk(2, []byte{1, 2, 3, 4, 5, 6, 7, 8}),
		mk(3, make([]byte, 64)),
	}
	raw := buildBlock(t, 1, "initminer", network.GenesisTimeUnix+3, txs)
	if _, _, err := block.UnmarshalSignedHeader(raw); err != nil {
		t.Fatalf("test block does not parse: %v", err)
	}
	return raw
}
