package chain

import "testing"

// TestSelectionBoundsAreSane pins the constants that keep block assembly inside
// the slot budget.
//
// Selection runs under the apply lock and costs a signature recovery plus a
// trial apply per candidate (~100µs+). With the mempool at its cap that is
// thousands of candidates — enough to consume the 3s slot we are trying to
// fill. Overrunning costs the block entirely, which is strictly worse than
// shipping a partial one.
func TestSelectionBoundsAreSane(t *testing.T) {
	// Must leave clear headroom inside a 3s slot for signing, publication and
	// the apply that follows.
	if maxSelectDuration >= 1500e6 {
		t.Fatalf("maxSelectDuration=%v leaves too little of the 3s slot for signing and apply", maxSelectDuration)
	}
	if maxSelectDuration <= 100e6 {
		t.Fatalf("maxSelectDuration=%v is too tight to fill a block", maxSelectDuration)
	}
	// A full block is ~65 KB; at a ~250 byte median transaction that is ~260
	// transactions, so the evaluation cap must be comfortably above that or we
	// would stop looking before the block is full.
	if maxEvaluatedFloor := 300; maxSelectEvaluated() < maxEvaluatedFloor {
		t.Fatalf("evaluation cap %d is below the ~%d transactions a full block holds", maxSelectEvaluated(), maxEvaluatedFloor)
	}
	if minTxBytes <= 0 || minTxBytes > 1024 {
		t.Fatalf("minTxBytes=%d is not a sane floor for a serialized transaction", minTxBytes)
	}
}
