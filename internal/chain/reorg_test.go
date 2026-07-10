package chain

import (
	"testing"

	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/network"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
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
