package chain

import (
	"testing"

	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/network"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
)

// TestPushLiveBlockPrunesForkDB is the regression test for an UNBOUNDED fork
// DAG. forkDB.add retains each block's raw bytes and forkDB.prune had no
// production caller whatsoever, so a live node accumulated every block it ever
// received -- ~28.8k blocks/day, with their decompressed payloads pinned.
//
// The pre-existing forkdb test exercised prune() directly, which is exactly why
// the bug survived: the function was correct, nothing invoked it. This test
// drives the real entry point (PushLiveBlock) instead and asserts the DAG stays
// bounded, so a future refactor that drops the call fails here.
func TestPushLiveBlockPrunesForkDB(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}
	c := New(db)
	opts := ApplyOptions{SkipTransactionSignatures: true, SkipBlockSignature: true, SkipDedupAndTaPOS: true}

	build := func(num uint32, prev block.ID) ([]byte, block.ID) {
		hdr := block.SignedHeader{Header: block.Header{
			Timestamp: network.GenesisTimeUnix + 3*num, Witness: "initminer",
			Previous: prev, Extensions: []byte{0x00},
		}}
		out := block.MarshalSignedHeader(nil, &hdr)
		out = append(out, byte(0))
		_, hdrLen, err := block.UnmarshalSignedHeader(out)
		if err != nil {
			t.Fatalf("header decode: %v", err)
		}
		return out, block.ConstructID(out[:hdrLen], num)
	}

	const total = forkKeepBelowLIB * 6
	prev, _ := c.HeadBlockID()
	for n := uint32(1); n <= total; n++ {
		raw, id := build(n, prev)
		if _, err := c.PushLiveBlock(raw, n, opts); err != nil {
			t.Fatalf("PushLiveBlock %d: %v", n, err)
		}
		prev = id
	}

	// A linear chain must leave only the retention window behind, not all of
	// history. Allow slack for the seed node and the pending reversible tip.
	got := c.ForkDBLen()
	if max := forkKeepBelowLIB + 8; got > max {
		t.Fatalf("fork DAG holds %d blocks after %d linear pushes, want <= %d "+
			"(prune is not running -- this is the unbounded-growth leak)", got, total, max)
	}
	if got == 0 {
		t.Fatalf("fork DAG empty: pruned the reversible window it needs to reorg")
	}

	// And it must still work: the head is the last block pushed.
	if hid, _ := c.HeadBlockID(); hid != prev {
		t.Fatalf("head=%x want %x after pruning", hid[:4], prev[:4])
	}
}
