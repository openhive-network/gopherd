package block

import "testing"

func TestKnownBadMerkleAccepted(t *testing.T) {
	// Block 3705111 — first known-bad. The algorithmic answer (left) and
	// the header value (right) should both be accepted as valid for that
	// block.
	headerStored := [20]byte{0xe6, 0xad, 0x56, 0x76, 0xbf, 0x6c, 0xb1, 0xa3, 0x93, 0x37, 0x6f, 0xd4, 0xd1, 0x5f, 0x43, 0xe9, 0xe7, 0x6a, 0x53, 0x5f}
	algorithmic := KnownBadMerkleRoots[3705111]

	if !MerkleRootAccepted(3705111, algorithmic, headerStored) {
		t.Error("known-bad merkle root must be accepted as algorithmic-correct")
	}
	if !MerkleRootAccepted(3705111, headerStored, headerStored) {
		t.Error("header-stored merkle root must always be accepted")
	}

	// A random other value for the same block must NOT be accepted.
	bogus := [20]byte{0xde, 0xad, 0xbe, 0xef}
	if MerkleRootAccepted(3705111, bogus, headerStored) {
		t.Error("random merkle must not be accepted")
	}

	// For a non-listed block (e.g. block 1), only header equality is OK.
	if MerkleRootAccepted(1, algorithmic, headerStored) {
		t.Error("non-listed blocks must not accept the known-bad value")
	}
}

func TestAllEightKnownBadBlocksPresent(t *testing.T) {
	expected := []uint32{3705111, 3705120, 3713940, 3714132, 3714567, 3714588, 4138790, 5435426}
	if len(KnownBadMerkleRoots) != len(expected) {
		t.Fatalf("expected %d entries, got %d", len(expected), len(KnownBadMerkleRoots))
	}
	for _, n := range expected {
		if _, ok := KnownBadMerkleRoots[n]; !ok {
			t.Errorf("block %d missing from KnownBadMerkleRoots", n)
		}
	}
}
