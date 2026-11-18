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

func newForkTestChain(t *testing.T) *Database {
	t.Helper()
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}
	s := state.NewSession(db)
	objects.PutAccount(s, &objects.Account{Name: "alice", Balance: 1000})
	objects.PutAccount(s, &objects.Account{Name: "bob", Balance: 0})
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}
	return New(db)
}

func aliceToBob(amount int64) tx.SignedTransaction {
	return tx.SignedTransaction{Transaction: tx.Transaction{
		Operations: op.Ops(op.Transfer{From: "alice", To: "bob", Amount: hiveAsset(amount), Memo: "m"}),
		Expiration: network.GenesisTimeUnix + 600,
	}}
}

// forkTestBlock builds a block whose header commits to headerTxs but whose
// body carries bodyTxs (the same slice for an honest block).
func forkTestBlock(t *testing.T, num, ts uint32, prev block.ID, headerTxs, bodyTxs []tx.SignedTransaction) ([]byte, block.ID) {
	t.Helper()
	hdr := block.SignedHeader{Header: block.Header{
		Timestamp: ts, Witness: "initminer", Previous: prev, Extensions: []byte{0x00},
		TransactionMerkleRoot: testMerkleRoot(headerTxs...),
	}}
	out := block.MarshalSignedHeader(nil, &hdr)
	out = append(out, byte(len(bodyTxs)))
	for i := range bodyTxs {
		out = tx.AppendSignedTransaction(out, &bodyTxs[i])
	}
	_, hdrLen, err := block.UnmarshalSignedHeader(out)
	if err != nil {
		t.Fatal(err)
	}
	return out, block.ConstructID(out[:hdrLen], num)
}

// hive#982: a copy of a block with its genuine header and a different body
// used to enter the fork DAG under the genuine id. The genuine copy then came
// back PushDuplicate and the head never moved past it.
func TestPushLiveBlockBadBodyDoesNotShadowGenuine(t *testing.T) {
	c := newForkTestChain(t)
	opts := ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true, SkipDedupAndTaPOS: true}
	gid, _ := c.HeadBlockID()

	r1, id1 := forkTestBlock(t, 1, network.GenesisTimeUnix+3, gid, nil, nil)
	if o, err := c.PushLiveBlock(r1, 1, opts); err != nil || o != PushExtended {
		t.Fatalf("block 1: %v %v", o, err)
	}

	honest := []tx.SignedTransaction{aliceToBob(250)}
	forged := []tx.SignedTransaction{aliceToBob(999)}
	bad, badID := forkTestBlock(t, 2, network.GenesisTimeUnix+6, id1, honest, forged)
	good, goodID := forkTestBlock(t, 2, network.GenesisTimeUnix+6, id1, honest, honest)
	if badID != goodID {
		t.Fatal("test setup: the forged copy must carry the genuine id")
	}

	if _, err := c.PushLiveBlock(bad, 2, opts); err == nil {
		t.Fatal("a block whose body does not match its header was accepted")
	}
	if _, ok := c.fork.get(goodID); ok {
		t.Fatal("the forged copy was stored in the fork DAG")
	}
	if o, err := c.PushLiveBlock(good, 2, opts); err != nil || o != PushExtended {
		t.Fatalf("genuine block 2 after a forged copy: outcome=%v err=%v, want extended", o, err)
	}
	if a, _ := objects.GetAccount(c.session, "alice"); a.Balance != 750 {
		t.Fatalf("alice=%d, want 750", a.Balance)
	}
}

// A block that fails to apply must leave the DAG, so its id is not answered
// as a duplicate from then on.
func TestPushLiveBlockFailedApplyLeavesDAG(t *testing.T) {
	c := newForkTestChain(t)
	opts := ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true, SkipDedupAndTaPOS: true}
	gid, _ := c.HeadBlockID()

	r1, id1 := forkTestBlock(t, 1, network.GenesisTimeUnix+3, gid, nil, nil)
	if _, err := c.PushLiveBlock(r1, 1, opts); err != nil {
		t.Fatal(err)
	}
	overdraw := []tx.SignedTransaction{aliceToBob(5000)} // alice holds 1000
	r2, id2 := forkTestBlock(t, 2, network.GenesisTimeUnix+6, id1, overdraw, overdraw)
	if _, err := c.PushLiveBlock(r2, 2, opts); err == nil {
		t.Fatal("an overdrawing block applied")
	}
	if _, ok := c.fork.get(id2); ok {
		t.Fatal("the failed block stayed in the fork DAG")
	}
	if o, err := c.PushLiveBlock(r2, 2, opts); o == PushDuplicate || err == nil {
		t.Fatalf("re-push of the failed block: outcome=%v err=%v, want a fresh failure", o, err)
	}
	if h, _ := c.HeadBlock(); h != 1 {
		t.Fatalf("head=%d, want 1", h)
	}
}

// The p2p apply paths set CheckPrevious, so a block numbered head+1 that does
// not build on the head is refused even with the block checks skipped.
func TestApplyBlockCheckPrevious(t *testing.T) {
	c := newForkTestChain(t)
	opts := ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true, SkipDedupAndTaPOS: true, CheckPrevious: true}
	gid, _ := c.HeadBlockID()
	r1, _ := forkTestBlock(t, 1, network.GenesisTimeUnix+3, gid, nil, nil)
	if _, err := c.ApplyBlock(r1, 1, opts, nil); err != nil {
		t.Fatal(err)
	}
	stray, _ := forkTestBlock(t, 2, network.GenesisTimeUnix+6, block.ID{0, 0, 0, 1, 0xee}, nil, nil)
	if _, err := c.ApplyBlock(stray, 2, opts, nil); err == nil {
		t.Fatal("block 2 that does not build on block 1 applied with CheckPrevious")
	}
}

// hived c8fd68ff5: a block recent enough that its transactions may still be
// unexpired keeps the duplicate check and its records even when the caller
// (gopherd replay) asked to skip them.
func TestRecentBlocksKeepDedupRecordsUnderSkip(t *testing.T) {
	c := newForkTestChain(t)
	gid, _ := c.HeadBlockID()
	st := aliceToBob(10)
	r1, _ := forkTestBlock(t, 1, network.GenesisTimeUnix+3, gid, []tx.SignedTransaction{st}, []tx.SignedTransaction{st})
	id := tx.IDFromDigest(tx.DigestFromBytes(tx.AppendTransactionBody(nil, &st.Transaction)))

	old := nowUnix
	defer func() { nowUnix = old }()
	nowUnix = func() int64 { return int64(network.GenesisTimeUnix) + 3600 } // the block is an hour old

	opts := ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true, SkipDedupAndTaPOS: true}
	if _, err := c.ApplyBlock(r1, 1, opts, nil); err != nil {
		t.Fatal(err)
	}
	if ok, _ := objects.HasTransaction(c.session, id); !ok {
		t.Fatal("no duplicate-check record for a transaction of a recent block applied with SkipDedupAndTaPOS")
	}
}
