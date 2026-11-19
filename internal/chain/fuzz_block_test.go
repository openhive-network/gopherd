package chain

import (
	"testing"

	"gopherd/internal/protocol/block"
	"gopherd/internal/testutil/corpus"
)

// FuzzBlockBody runs every routine that splits a block received from a peer
// before it is applied: merkle-root check, transaction bodies and ids, and
// the parallel signer-key recovery. A peer controls these bytes, so none may
// panic, and the transaction splitters must agree on how many transactions
// the block has.
//
// Seeds under testdata/fuzz/FuzzBlockBody are small mainnet blocks with
// transactions (internal/fuzzseed).
func FuzzBlockBody(f *testing.F) {
	f.Add(make([]byte, 20+4+1+20+1+65+1))
	f.Fuzz(func(t *testing.T, raw []byte) {
		num := uint32(1)
		if sh, _, err := block.UnmarshalSignedHeader(raw); err == nil {
			num = sh.Previous.Num() + 1
		}
		_ = CheckMerkleRoot(raw, num)
		bodies, err := BlockTransactionBodies(raw)
		ids := BlockTransactionIDs(raw)
		if err == nil && ids != nil && len(ids) != len(bodies) {
			t.Fatalf("%d transaction bodies but %d ids", len(bodies), len(ids))
		}
		_ = RecoverBlockTxSignerKeys(raw, num)
	})
}

// TestBlockBodySeedsCheck pins that every mainnet block in the seed corpus
// passes the receive-path merkle check.
func TestBlockBodySeedsCheck(t *testing.T) {
	seeds := corpus.Load(t, "FuzzBlockBody")
	if len(seeds) == 0 {
		t.Skip("no seed corpus")
	}
	for _, raw := range seeds {
		sh, _, err := block.UnmarshalSignedHeader(raw)
		if err != nil {
			t.Fatalf("seed header: %v", err)
		}
		num := sh.Previous.Num() + 1
		if err := CheckMerkleRoot(raw, num); err != nil {
			t.Fatalf("block %d: %v", num, err)
		}
		bodies, err := BlockTransactionBodies(raw)
		if err != nil || len(bodies) == 0 {
			t.Fatalf("block %d: %d bodies, err %v", num, len(bodies), err)
		}
	}
}
