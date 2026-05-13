package tx

import (
	"testing"

	"hived-go/internal/crypto"
)

// TestMerkleRootIncrementalGrowth ports basic_tests.cpp::merkle_root.
// Builds a sequence of per-tx digests and verifies the merkle root at
// each step matches the C++ reference algorithm:
//
//	leaf:    d_i = sha256(serialized_tx_i)
//	pairs:   d(L,R) = sha256(L || R)
//	odd:     last leaf carries through
//	final:   ripemd160(top sha256)  // applied via tx.MerkleRoot
//
// We construct synthetic per-tx digests directly (the C++ test pulls
// them from `full_transaction_type::get_merkle_digest()` of empty txs
// with a varying ref_block_prefix; the value isn't important — only
// that the reduction tree shape matches).
func TestMerkleRootIncrementalGrowth(t *testing.T) {
	// 10 distinct leaf digests.
	leaves := make([]Digest, 10)
	for i := range leaves {
		leaves[i] = Digest(crypto.SHA256([]byte{byte(i)}))
	}
	d := func(l, r Digest) Digest {
		var buf [64]byte
		copy(buf[:32], l[:])
		copy(buf[32:], r[:])
		return Digest(crypto.SHA256(buf[:]))
	}
	c := func(top Digest) [20]byte {
		return crypto.RIPEMD160(top[:])
	}

	// Empty list → all-zero 20-byte checksum.
	if got := MerkleRoot(nil); got != ([20]byte{}) {
		t.Errorf("empty merkle root: got %x, want zero", got)
	}

	// 1 tx → root = ripemd160(leaf[0]).
	got := MerkleRoot(leaves[:1])
	want := c(leaves[0])
	if got != want {
		t.Errorf("1-tx merkle: got %x, want %x", got, want)
	}

	// 2 tx → root = ripemd160(d(0,1)).
	dA := d(leaves[0], leaves[1])
	if got := MerkleRoot(leaves[:2]); got != c(dA) {
		t.Errorf("2-tx merkle: got %x, want %x", got, c(dA))
	}

	// 3 tx — odd-count carry-through:
	// level 1: d(0,1), 2     (the odd 2 carries up)
	// level 2: d(d(0,1), 2)  → root.
	// In C++ that's d(A,B) where B = leaves[2].
	dI := d(dA, leaves[2])
	if got := MerkleRoot(leaves[:3]); got != c(dI) {
		t.Errorf("3-tx merkle: got %x, want %x", got, c(dI))
	}

	// 4 tx — fully balanced:
	// level 1: d(0,1), d(2,3)
	// level 2: d(.,.) → root.
	dB := d(leaves[2], leaves[3])
	dI = d(dA, dB)
	if got := MerkleRoot(leaves[:4]); got != c(dI) {
		t.Errorf("4-tx merkle: got %x, want %x", got, c(dI))
	}

	// 5 tx — odd at level 1; carry-through then carry-through again:
	// level 1: d(0,1), d(2,3), 4
	// level 2: d(d(0,1), d(2,3)), 4   → 4 carries
	// level 3: d(., 4)  → root.
	dC := leaves[4]
	dM := d(dI, dC)
	if got := MerkleRoot(leaves[:5]); got != c(dM) {
		t.Errorf("5-tx merkle: got %x, want %x", got, c(dM))
	}

	// 6 tx — odd at level 1 carries:
	// level 1: d(0,1), d(2,3), d(4,5)
	// level 2: d(d(0,1), d(2,3)), d(4,5) -- 3 nodes, last carries
	// level 3: d(.,.) → root.
	dC = d(leaves[4], leaves[5])
	dM = d(dI, dC)
	if got := MerkleRoot(leaves[:6]); got != c(dM) {
		t.Errorf("6-tx merkle: got %x, want %x", got, c(dM))
	}

	// 7 tx — odd at level 2, carry-through:
	// level 1: d(0,1), d(2,3), d(4,5), 6
	// level 2: d(d(0,1), d(2,3)), d(d(4,5), 6)
	// level 3: d(.,.) → root.
	dD := leaves[6]
	dJ := d(dC, dD)
	dM = d(dI, dJ)
	if got := MerkleRoot(leaves[:7]); got != c(dM) {
		t.Errorf("7-tx merkle: got %x, want %x", got, c(dM))
	}

	// 8 tx — fully balanced 3-deep tree.
	dD = d(leaves[6], leaves[7])
	dJ = d(dC, dD)
	dM = d(dI, dJ)
	if got := MerkleRoot(leaves[:8]); got != c(dM) {
		t.Errorf("8-tx merkle: got %x, want %x", got, c(dM))
	}

	// 9 tx — leaf 8 carries through three levels:
	// level 1: d(0,1), d(2,3), d(4,5), d(6,7), 8
	// level 2: d(d(0,1), d(2,3)), d(d(4,5), d(6,7)), 8 (8 carries)
	// level 3: d(.,.), 8           (8 still carries)
	// level 4: d(., 8)             → root.
	dE := leaves[8]
	dK := dE
	dN := dK
	dO := d(dM, dN)
	if got := MerkleRoot(leaves[:9]); got != c(dO) {
		t.Errorf("9-tx merkle: got %x, want %x", got, c(dO))
	}

	// 10 tx — odd at level 2 only:
	// level 1: d(0,1), d(2,3), d(4,5), d(6,7), d(8,9)
	// level 2: d(d(0,1), d(2,3)), d(d(4,5), d(6,7)), d(8,9) (last carries)
	// level 3: d(.,.), d(8,9)      (carries)
	// level 4: d(., d(8,9))        → root.
	dE = d(leaves[8], leaves[9])
	dK = dE
	dN = dK
	dO = d(dM, dN)
	if got := MerkleRoot(leaves[:10]); got != c(dO) {
		t.Errorf("10-tx merkle: got %x, want %x", got, c(dO))
	}
}
