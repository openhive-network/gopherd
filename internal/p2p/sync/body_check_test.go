package sync

import (
	"testing"

	"gopherd/internal/protocol/block"
)

func TestCheckBlockBody(t *testing.T) {
	raw, id := buildTestSignedBlock(t, 7, "initminer") // zero merkle root, no transactions
	if r := checkBlockBody(raw, id); r != "" {
		t.Fatalf("genuine block refused: %s", r)
	}
	if r := checkBlockBody(raw, block.ID{}); r != "" {
		t.Fatalf("genuine block without a wire id refused: %s", r)
	}
	other := id
	other[19] ^= 1
	if checkBlockBody(raw, other) == "" {
		t.Fatal("block accepted under an id its header does not hash to")
	}
	// Same header, but the body now claims one (empty) transaction: the zero
	// merkle root no longer matches.
	forged := append(append([]byte{}, raw[:len(raw)-1]...), 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	if checkBlockBody(forged, id) == "" {
		t.Fatal("block whose body does not match its merkle root accepted")
	}
}
