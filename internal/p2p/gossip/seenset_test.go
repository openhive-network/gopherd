package gossip

import (
	"testing"

	"gopherd/internal/protocol/block"
)

// TestSeenSetAddContains exercises the basic add/contains behavior:
// fresh ID returns true on add, second add returns false (dedup hit),
// contains reflects the inserted set.
func TestSeenSetAddContains(t *testing.T) {
	s := newSeenSet(8)
	var id1 block.ID
	id1[0] = 0x11
	var id2 block.ID
	id2[0] = 0x22

	if !s.add(id1) {
		t.Fatalf("first add should return true")
	}
	if s.add(id1) {
		t.Fatalf("second add of same ID should return false (dedup)")
	}
	if !s.contains(id1) {
		t.Fatalf("id1 should be present")
	}
	if s.contains(id2) {
		t.Fatalf("id2 should NOT be present yet")
	}
	if !s.add(id2) {
		t.Fatalf("first add of id2 should return true")
	}
}

// TestSeenSetEvictsOldest verifies the FIFO eviction policy: once we
// blow past the capacity, the oldest entries are evicted in insertion
// order (so an ID we added FIRST is no longer .contains() after we've
// added cap+1 items).
func TestSeenSetEvictsOldest(t *testing.T) {
	const cap = 4
	s := newSeenSet(cap)
	ids := make([]block.ID, cap+2)
	for i := range ids {
		ids[i][0] = byte(i + 1)
		s.add(ids[i])
	}
	// Capacity 4, inserted 6 — first two should be evicted.
	if s.contains(ids[0]) {
		t.Errorf("ids[0] should be evicted (oldest)")
	}
	if s.contains(ids[1]) {
		t.Errorf("ids[1] should be evicted")
	}
	for i := 2; i < len(ids); i++ {
		if !s.contains(ids[i]) {
			t.Errorf("ids[%d] should still be present", i)
		}
	}
}
