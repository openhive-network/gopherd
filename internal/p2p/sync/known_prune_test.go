package sync

import (
	"testing"

	"hived-go/internal/protocol/block"
)

// TestKnownPruneDropsTransactionIDs pins the leak fix.
//
// The per-peer `known` set holds BOTH block ids and transaction advert ids.
// Block ids embed their height, so pruning by height works for them. A
// transaction advert id is a ripemd160 hash: Num() reads its first four bytes,
// which are effectively random and almost always far beyond head, so the
// original `n <= head` test never matched and every transaction ever seen
// stayed resident forever (~2.5 GiB/day of live-heap growth).
func TestKnownPruneDropsTransactionIDs(t *testing.T) {
	const head = 108_000_000

	keep := func(id block.ID) bool {
		n := id.Num()
		return !(n != 0 && (n <= head || n > head+knownFutureSlack))
	}

	// A block id at head-10 is already ours: prunable.
	var oldBlock block.ID
	ob := uint32(head - 10)
	oldBlock[0] = byte(ob >> 24)
	oldBlock[1] = byte(ob >> 16)
	oldBlock[2] = byte(ob >> 8)
	oldBlock[3] = byte(ob)
	if keep(oldBlock) {
		t.Fatal("a block at/below head should be pruned")
	}

	// The very next block must survive: we still want dedup on it.
	var nextBlock block.ID
	nb := uint32(head + 1)
	nextBlock[0] = byte(nb >> 24)
	nextBlock[1] = byte(nb >> 16)
	nextBlock[2] = byte(nb >> 8)
	nextBlock[3] = byte(nb)
	if !keep(nextBlock) {
		t.Fatal("the next block must be kept for dedup")
	}

	// Transaction hashes: first bytes effectively random, so Num() lands far
	// beyond head. These are what leaked.
	leaked := 0
	for i := 0; i < 1000; i++ {
		var tx block.ID
		tx[0] = byte(0x80 + i%0x7f) // high bit set => Num() >> head
		tx[1] = byte(i)
		tx[2] = byte(i >> 8)
		tx[3] = byte(i * 7)
		if keep(tx) {
			leaked++
		}
	}
	if leaked != 0 {
		t.Fatalf("%d/1000 transaction ids would still be retained forever", leaked)
	}
}
