package chain

import (
	"testing"

	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/network"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

// TestPushLiveBlockReorg drives a depth-1 tip reorg entirely offline through
// PushLiveBlock: build chain A (…-1-2A) then a strictly-longer competing chain B
// (…-1-2B-3B); the node must UNDO its uncommitted tip 2A and switch to B, with
// balances reflecting B's transfers, not A's.
func TestPushLiveBlockReorg(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}
	{
		s := state.NewSession(db)
		objects.PutAccount(s, &objects.Account{Name: "alice", Balance: 1000})
		objects.PutAccount(s, &objects.Account{Name: "bob", Balance: 0})
		if err := s.Commit(false); err != nil {
			t.Fatal(err)
		}
	}
	c := New(db)
	opts := ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true, SkipDedupAndTaPOS: true}

	// buildAt builds a block at height num with an explicit previous id and an
	// optional alice->bob transfer, returning the raw bytes and the block id.
	buildAt := func(num uint32, ts uint32, prev block.ID, amount int64) ([]byte, block.ID) {
		var txs []tx.SignedTransaction
		if amount > 0 {
			txs = append(txs, tx.SignedTransaction{Transaction: tx.Transaction{
				Operations: op.Ops(op.Transfer{From: "alice", To: "bob", Amount: hiveAsset(amount), Memo: "m"}),
				Expiration: network.GenesisTimeUnix + 600,
			}})
		}
		hdr := block.SignedHeader{Header: block.Header{
			Timestamp: ts, Witness: "initminer", Previous: prev, Extensions: []byte{0x00},
		}}
		out := block.MarshalSignedHeader(nil, &hdr)
		out = append(out, byte(len(txs)))
		for i := range txs {
			out = tx.AppendSignedTransaction(out, &txs[i])
		}
		_, hdrLen, err := block.UnmarshalSignedHeader(out)
		if err != nil {
			t.Fatalf("build header decode: %v", err)
		}
		return out, block.ConstructID(out[:hdrLen], num)
	}
	bal := func(name string) int64 { a, _ := objects.GetAccount(c.session, name); return a.Balance }
	push := func(raw []byte, num uint32) PushOutcome {
		out, err := c.PushLiveBlock(raw, num, opts)
		if err != nil {
			t.Fatalf("PushLiveBlock %d: %v", num, err)
		}
		return out
	}

	gid, _ := c.HeadBlockID() // committed head before block 1 (genesis)

	// Block 1 (no transfer) extends genesis.
	r1, id1 := buildAt(1, network.GenesisTimeUnix+3, gid, 0)
	if o := push(r1, 1); o != PushExtended {
		t.Fatalf("block1 outcome=%v want extended", o)
	}
	// Block 2A: alice->bob 250. Extends block1 (finalizes block1, 2A pending).
	r2a, _ := buildAt(2, network.GenesisTimeUnix+6, id1, 250)
	if o := push(r2a, 2); o != PushExtended {
		t.Fatalf("block2A outcome=%v want extended", o)
	}
	if bal("alice") != 750 || bal("bob") != 250 {
		t.Fatalf("chain-A tip balances alice=%d bob=%d want 750,250", bal("alice"), bal("bob"))
	}

	// Competing block 2B: alice->bob 100, same parent (block1) as 2A. Tie at
	// height 2 → buffered, head stays on 2A.
	r2b, id2b := buildAt(2, network.GenesisTimeUnix+7, id1, 100)
	if o := push(r2b, 2); o != PushBuffered {
		t.Fatalf("block2B outcome=%v want buffered", o)
	}
	if bal("alice") != 750 {
		t.Fatalf("balance must be unchanged after buffering 2B: alice=%d want 750", bal("alice"))
	}

	// Block 3B: alice->bob 50, builds on 2B → branch B height 3 > head height 2.
	// This triggers the reorg: undo 2A, apply 2B + 3B.
	r3b, id3b := buildAt(3, network.GenesisTimeUnix+9, id2b, 50)
	if o := push(r3b, 3); o != PushReorged {
		t.Fatalf("block3B outcome=%v want reorged", o)
	}
	// Chain B won: alice = 1000-100-50 = 850, bob = 150. (NOT chain A's 750/250.)
	if bal("alice") != 850 || bal("bob") != 150 {
		t.Fatalf("post-reorg balances alice=%d bob=%d want 850,150 (chain B)", bal("alice"), bal("bob"))
	}
	// Head is now 3B.
	if hid, _ := c.HeadBlockID(); hid != id3b {
		t.Fatalf("post-reorg head=%x want 3B=%x", hid[:4], id3b[:4])
	}
}

// TestPushLiveBlockOutOfOrder checks the drain-forward engine: a block that
// arrives BEFORE its parent is buffered, and once the parent lands both apply.
func TestPushLiveBlockOutOfOrder(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}
	{
		s := state.NewSession(db)
		objects.PutAccount(s, &objects.Account{Name: "alice", Balance: 1000})
		objects.PutAccount(s, &objects.Account{Name: "bob", Balance: 0})
		_ = s.Commit(false)
	}
	c := New(db)
	opts := ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true, SkipDedupAndTaPOS: true}
	buildAt := func(num, ts uint32, prev block.ID, amount int64) ([]byte, block.ID) {
		st := tx.SignedTransaction{Transaction: tx.Transaction{
			Operations: op.Ops(op.Transfer{From: "alice", To: "bob", Amount: hiveAsset(amount), Memo: "m"}),
			Expiration: network.GenesisTimeUnix + 600,
		}}
		hdr := block.SignedHeader{Header: block.Header{Timestamp: ts, Witness: "initminer", Previous: prev, Extensions: []byte{0x00}}}
		out := block.MarshalSignedHeader(nil, &hdr)
		out = append(out, 1)
		out = tx.AppendSignedTransaction(out, &st)
		_, hl, _ := block.UnmarshalSignedHeader(out)
		return out, block.ConstructID(out[:hl], num)
	}
	bal := func(n string) int64 { a, _ := objects.GetAccount(c.session, n); return a.Balance }

	gid, _ := c.HeadBlockID()
	r1, id1 := buildAt(1, network.GenesisTimeUnix+3, gid, 100)
	r2, _ := buildAt(2, network.GenesisTimeUnix+6, id1, 50)

	// Block 2 arrives FIRST → buffered, no state change.
	if o, err := c.PushLiveBlock(r2, 2, opts); err != nil || o != PushBuffered {
		t.Fatalf("early block2 outcome=%v err=%v want buffered", o, err)
	}
	if bal("alice") != 1000 {
		t.Fatalf("buffered block must not mutate state: alice=%d", bal("alice"))
	}
	// Block 1 lands → drain-forward applies 1 then 2.
	if o, err := c.PushLiveBlock(r1, 1, opts); err != nil || o != PushExtended {
		t.Fatalf("block1 outcome=%v err=%v want extended", o, err)
	}
	if bal("alice") != 850 || bal("bob") != 150 {
		t.Fatalf("after drain: alice=%d bob=%d want 850,150 (both blocks applied)", bal("alice"), bal("bob"))
	}
	if hn, _ := c.HeadBlock(); hn != 2 {
		t.Fatalf("head=%d want 2", hn)
	}
}

// TestReversibleFinalizeCallbacks verifies the Phase-2 side-effect routing: a
// reversible (uncommitted tip) block fires NEITHER onBlockApplied NOR
// onBlockFinalized; FinalizeReversible fires onBlockFinalized (with the block's
// raw bytes); UndoReversible fires neither (an orphaned tip must not
// block_log-append or evict); a non-reversible apply fires onBlockApplied.
func TestReversibleFinalizeCallbacks(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}
	{
		s := state.NewSession(db)
		objects.PutAccount(s, &objects.Account{Name: "alice", Balance: 1000})
		objects.PutAccount(s, &objects.Account{Name: "bob", Balance: 0})
		_ = s.Commit(false)
	}
	c := New(db)

	var applied, finalized []uint32
	var finalizedRaw [][]byte
	c.SetOnBlockApplied(func(n uint32, _ block.ID) { applied = append(applied, n) })
	c.SetOnBlockFinalized(func(n uint32, raw []byte) {
		finalized = append(finalized, n)
		finalizedRaw = append(finalizedRaw, raw)
	})

	mk := func(num uint32, ts uint32, amount int64) []byte {
		st := tx.SignedTransaction{Transaction: tx.Transaction{
			Operations: op.Ops(op.Transfer{From: "alice", To: "bob", Amount: hiveAsset(amount), Memo: "m"}),
			Expiration: network.GenesisTimeUnix + 600,
		}}
		return buildBlock(t, num, "initminer", ts, []tx.SignedTransaction{st})
	}
	rev := ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true, SkipDedupAndTaPOS: true, Reversible: true}
	norm := ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true, SkipDedupAndTaPOS: true}

	// Reversible apply → neither callback fires yet.
	raw1 := mk(1, network.GenesisTimeUnix+3, 10)
	if _, err := c.ApplyBlock(raw1, 1, rev, nil); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 || len(finalized) != 0 {
		t.Fatalf("reversible apply must fire no callback: applied=%v finalized=%v", applied, finalized)
	}
	// Finalize → onBlockFinalized fires with the block's raw bytes.
	if err := c.FinalizeReversible(false); err != nil {
		t.Fatal(err)
	}
	if len(finalized) != 1 || finalized[0] != 1 || string(finalizedRaw[0]) != string(raw1) {
		t.Fatalf("finalize should fire onBlockFinalized(1, raw1); got %v", finalized)
	}
	if len(applied) != 0 {
		t.Fatalf("onBlockApplied must not fire for the reversible path: %v", applied)
	}

	// Reversible apply then UNDO (orphaned tip) → still no new side-effects.
	if _, err := c.ApplyBlock(mk(2, network.GenesisTimeUnix+6, 20), 2, rev, nil); err != nil {
		t.Fatal(err)
	}
	c.UndoReversible()
	if len(finalized) != 1 || len(applied) != 0 {
		t.Fatalf("undo must fire nothing: applied=%v finalized=%v", applied, finalized)
	}

	// Non-reversible apply → onBlockApplied fires (unchanged replay path).
	if _, err := c.ApplyBlock(mk(2, network.GenesisTimeUnix+6, 30), 2, norm, nil); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0] != 2 {
		t.Fatalf("non-reversible apply should fire onBlockApplied(2); got %v", applied)
	}
}
