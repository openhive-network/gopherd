package chain

import (
	"testing"

	"hived-go/internal/chain/evaluator"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/network"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
)

// buildBlock constructs a minimal valid block payload (signed_header +
// transactions) for the apply pipeline tests.
func buildBlock(t *testing.T, blockNum uint32, witness string, ts uint32, txs []tx.SignedTransaction) []byte {
	t.Helper()
	hdr := block.SignedHeader{
		Header: block.Header{
			Timestamp:  ts,
			Witness:    witness,
			Extensions: []byte{0x00},
		},
	}
	out := block.MarshalSignedHeader(nil, &hdr)
	// Transactions vector: varint count + each signed_transaction.
	out = append(out, byte(len(txs))) // assumes count < 128
	for _, st := range txs {
		out = tx.AppendSignedTransaction(out, &st)
	}
	_ = blockNum
	return out
}

func TestApplyBlockEmpty(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatalf("genesis: %v", err)
	}

	chain := New(db)
	raw := buildBlock(t, 1, "initminer", 1_700_000_000, nil)
	if _, err := chain.ApplyBlock(raw, 1, ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true}, nil); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}
	head, err := chain.HeadBlock()
	if err != nil {
		t.Fatal(err)
	}
	if head != 1 {
		t.Fatalf("HeadBlock = %d, want 1", head)
	}

	s := state.NewSession(db)
	p, _ := objects.GetDGPO(s)
	if p.CurrentWitness != "initminer" {
		t.Errorf("CurrentWitness = %q, want initminer", p.CurrentWitness)
	}
}

func TestApplyBlockWithTransfer(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}

	// Seed a second account so the transfer evaluator finds both.
	{
		s := state.NewSession(db)
		objects.PutAccount(s, &objects.Account{Name: "alice", Balance: 1000})
		objects.PutAccount(s, &objects.Account{Name: "bob", Balance: 0})
		if err := s.Commit(false); err != nil {
			t.Fatal(err)
		}
	}

	chain := New(db)

	transferOp := op.Transfer{
		From:   "alice",
		To:     "bob",
		Amount: hiveAsset(250),
		Memo:   "lunch",
	}
	// Expiration must lie inside [head_block_time, +1h]; for block 1
	// dgpo.Time is genesis_time, so any value within that hour works.
	st := tx.SignedTransaction{Transaction: tx.Transaction{
		Operations: op.Ops(transferOp),
		Expiration: network.GenesisTimeUnix + 60,
	}}
	raw := buildBlock(t, 1, "initminer", network.GenesisTimeUnix+3, []tx.SignedTransaction{st})

	if _, err := chain.ApplyBlock(raw, 1, ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true}, nil); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}

	s := state.NewSession(db)
	a, _ := objects.GetAccount(s, "alice")
	b, _ := objects.GetAccount(s, "bob")
	if a.Balance != 750 || b.Balance != 250 {
		t.Fatalf("post-transfer balances: alice=%d bob=%d (want 750, 250)", a.Balance, b.Balance)
	}
}

func TestApplyBlockStubsRunWithoutError(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}

	// A custom_json op is fully covered by a registered (no-op) stub
	// — applying the block should succeed without ErrNotImplemented.
	customOp := op.CustomJSON{
		RequiredAuths:        nil,
		RequiredPostingAuths: []string{"initminer"},
		ID:                   "test",
		JSON:                 `{"k":1}`,
	}
	st := tx.SignedTransaction{Transaction: tx.Transaction{
		Operations: op.Ops(customOp),
		Expiration: network.GenesisTimeUnix + 60,
	}}
	raw := buildBlock(t, 1, "initminer", network.GenesisTimeUnix+3, []tx.SignedTransaction{st})

	chain := New(db)
	if _, err := chain.ApplyBlock(raw, 1, ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true}, nil); err != nil {
		t.Fatalf("ApplyBlock with stub custom_json: %v", err)
	}
}

func TestEvaluatorCoverageIsComplete(t *testing.T) {
	tags := evaluator.Coverage()
	if len(tags) < int(op.TagFirstVirtual) {
		t.Fatalf("evaluator coverage = %d, want >= %d (every regular op)", len(tags), op.TagFirstVirtual)
	}
}
