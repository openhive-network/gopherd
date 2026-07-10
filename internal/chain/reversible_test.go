package chain

import (
	"testing"

	"hived-go/internal/protocol/network"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
)

// TestReversibleApplyUndoFinalize exercises the Phase-1 reversible primitive:
// a block applied under ApplyOptions.Reversible is uncommitted (readable via the
// live session, invisible to the backend); UndoReversible restores the last
// committed state exactly; a competing block can then be applied and finalized.
// This is the state foundation for fork switching (undo the losing tip block,
// apply the winning branch).
func TestReversibleApplyUndoFinalize(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}
	// Seed two accounts + commit (this is the irreversible baseline).
	{
		s := state.NewSession(db)
		objects.PutAccount(s, &objects.Account{Name: "alice", Balance: 1000})
		objects.PutAccount(s, &objects.Account{Name: "bob", Balance: 0})
		if err := s.Commit(false); err != nil {
			t.Fatal(err)
		}
	}
	c := New(db)

	mkBlock := func(amount int64) []byte {
		st := tx.SignedTransaction{Transaction: tx.Transaction{
			Operations: op.Ops(op.Transfer{From: "alice", To: "bob", Amount: hiveAsset(amount), Memo: "m"}),
			Expiration: network.GenesisTimeUnix + 60,
		}}
		return buildBlock(t, 1, "initminer", network.GenesisTimeUnix+3, []tx.SignedTransaction{st})
	}
	revOpts := ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true, Reversible: true}

	// bal reads a balance through the chain's live session (accounts are a
	// shared in-memory subsystem store; the undo log is what makes them
	// reversible, not overlay isolation).
	bal := func(name string) int64 {
		a, _ := objects.GetAccount(c.session, name)
		return a.Balance
	}
	// headNum reads dgpo.HeadBlockNumber (K/V-backed) — verifies the overlay
	// side reverts too, not just the subsystem side.
	headNum := func() uint32 {
		p, _ := objects.GetDGPO(c.session)
		return p.HeadBlockNumber
	}
	baseHead := headNum()

	// 1) Apply a reversible transfer of 250: state mutates, block is pending.
	if _, err := c.ApplyBlock(mkBlock(250), 1, revOpts, nil); err != nil {
		t.Fatalf("reversible ApplyBlock: %v", err)
	}
	if _, _, _, ok := c.ReversiblePending(); !ok {
		t.Fatal("expected a pending reversible block")
	}
	if bal("alice") != 750 || bal("bob") != 250 {
		t.Fatalf("post-apply balances: alice=%d bob=%d want 750,250", bal("alice"), bal("bob"))
	}
	if headNum() != 1 {
		t.Fatalf("post-apply head=%d want 1", headNum())
	}

	// 2) Undo (fork switch): BOTH the subsystem (balances) AND K/V (head) revert
	// to the exact committed baseline.
	c.UndoReversible()
	if _, _, _, ok := c.ReversiblePending(); ok {
		t.Fatal("pending should be cleared after undo")
	}
	if bal("alice") != 1000 || bal("bob") != 0 {
		t.Fatalf("post-undo balances: alice=%d bob=%d want 1000,0", bal("alice"), bal("bob"))
	}
	if headNum() != baseHead {
		t.Fatalf("post-undo head=%d want %d (baseline)", headNum(), baseHead)
	}

	// 3) Apply the COMPETING branch block (transfer 100) reversibly, then finalize.
	if _, err := c.ApplyBlock(mkBlock(100), 1, revOpts, nil); err != nil {
		t.Fatalf("competing reversible ApplyBlock: %v", err)
	}
	if err := c.FinalizeReversible(false); err != nil {
		t.Fatalf("FinalizeReversible: %v", err)
	}
	if _, _, _, ok := c.ReversiblePending(); ok {
		t.Fatal("pending should be cleared after finalize")
	}
	// The competing branch won and is now committed.
	if bal("alice") != 900 || bal("bob") != 100 {
		t.Fatalf("post-finalize balances: alice=%d bob=%d want 900,100", bal("alice"), bal("bob"))
	}
	if headNum() != 1 {
		t.Fatalf("post-finalize head=%d want 1", headNum())
	}
}
