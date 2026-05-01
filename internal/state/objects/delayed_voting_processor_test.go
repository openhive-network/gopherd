package objects

import "testing"

func cmpDV(t *testing.T, items []DelayedVoteEntry, idx int, time uint32, val uint64) {
	t.Helper()
	if idx >= len(items) {
		t.Fatalf("items[%d] out of range (len=%d)", idx, len(items))
	}
	if items[idx].Time != time || items[idx].Val != val {
		t.Fatalf("items[%d]: got {%d, %d}, want {%d, %d}",
			idx, items[idx].Time, items[idx].Val, time, val)
	}
}

// Port of delayed_voting_tests.cpp::delayed_voting_processor_01.
//
// Exercises the bucket-coalesce semantics: add() folds into the back
// bucket if within DelayedVotingIntervalSec, otherwise appends.
// erase_front() drops the oldest.
func TestDelayedVotingProcessor01(t *testing.T) {
	var items []DelayedVoteEntry
	var sum uint64
	const min = uint32(60)
	interval := DelayedVotingIntervalSec
	base := uint32(946684800) // 2000-01-01T00:00:00 UTC

	if err := DelayedVotingAdd(&items, &sum, base+min, 1); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || sum != 1 {
		t.Fatalf("after add#1: len=%d sum=%d", len(items), sum)
	}
	cmpDV(t, items, 0, base+min, 1)

	if err := DelayedVotingAdd(&items, &sum, base+min+30*min, 2); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || sum != 3 {
		t.Fatalf("after add#2: len=%d sum=%d", len(items), sum)
	}
	cmpDV(t, items, 0, base+min, 3)

	if err := DelayedVotingAdd(&items, &sum, base+min+interval-3, 3); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || sum != 6 {
		t.Fatalf("after add#3: len=%d sum=%d", len(items), sum)
	}
	cmpDV(t, items, 0, base+min, 6)

	if err := DelayedVotingAdd(&items, &sum, base+min+interval, 4); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || sum != 10 {
		t.Fatalf("after add#4: len=%d sum=%d", len(items), sum)
	}
	cmpDV(t, items, 0, base+min, 6)
	cmpDV(t, items, 1, base+min+interval, 4)

	if err := DelayedVotingAdd(&items, &sum, base+min+2*interval, 5); err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || sum != 15 {
		t.Fatalf("after add#5: len=%d sum=%d", len(items), sum)
	}

	if err := DelayedVotingEraseFront(&items, &sum); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || sum != 9 {
		t.Fatalf("after erase#1: len=%d sum=%d", len(items), sum)
	}
	cmpDV(t, items, 0, base+min+interval, 4)
	cmpDV(t, items, 1, base+min+2*interval, 5)

	if err := DelayedVotingEraseFront(&items, &sum); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || sum != 5 {
		t.Fatalf("after erase#2: len=%d sum=%d", len(items), sum)
	}
	cmpDV(t, items, 0, base+min+2*interval, 5)

	if err := DelayedVotingAdd(&items, &sum, base+2*min+2*interval, 6); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || sum != 11 {
		t.Fatalf("after add#6: len=%d sum=%d", len(items), sum)
	}
	cmpDV(t, items, 0, base+min+2*interval, 11)

	if err := DelayedVotingAdd(&items, &sum, base+min+3*interval, 7); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || sum != 18 {
		t.Fatalf("after add#7: len=%d sum=%d", len(items), sum)
	}

	if err := DelayedVotingAdd(&items, &sum, base+min+3*interval+3, 8); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || sum != 26 {
		t.Fatalf("after add#8: len=%d sum=%d", len(items), sum)
	}
	cmpDV(t, items, 1, base+min+3*interval, 15)

	_ = DelayedVotingEraseFront(&items, &sum)
	_ = DelayedVotingEraseFront(&items, &sum)
	if len(items) != 0 || sum != 0 {
		t.Fatalf("after drain: len=%d sum=%d", len(items), sum)
	}

	// Multiple erase_front on empty: no-op.
	_ = DelayedVotingEraseFront(&items, &sum)
	_ = DelayedVotingEraseFront(&items, &sum)
	if len(items) != 0 || sum != 0 {
		t.Fatalf("after empty erases: len=%d sum=%d", len(items), sum)
	}

	// Sanity: erase_front when empty + sum != 0 → error.
	sum = 1
	if err := DelayedVotingEraseFront(&items, &sum); err == nil {
		t.Fatal("erase_front on empty with sum=1 should error")
	}
	sum = 0

	_ = DelayedVotingAdd(&items, &sum, base, 2)
	sum--
	if err := DelayedVotingEraseFront(&items, &sum); err == nil {
		t.Fatal("erase_front with sum < front.val should error")
	}
	sum++
	_ = DelayedVotingEraseFront(&items, &sum)
	if len(items) != 0 || sum != 0 {
		t.Fatalf("final drain: len=%d sum=%d", len(items), sum)
	}

	// Time-regression rejection.
	if err := DelayedVotingAdd(&items, &sum, base+30, 1000); err != nil {
		t.Fatal(err)
	}
	if err := DelayedVotingAdd(&items, &sum, base+29, 1000); err == nil {
		t.Fatal("add with regressed head_time should error")
	}
}

// Port of delayed_voting_tests.cpp::delayed_voting_processor_02.
// Tests the erase() function which removes votes from the BACK,
// popping/decrementing buckets until count is consumed.
func TestDelayedVotingProcessor02(t *testing.T) {
	var items []DelayedVoteEntry
	var sum uint64
	base := uint32(946684800)
	interval := DelayedVotingIntervalSec

	_ = DelayedVotingAdd(&items, &sum, base, 5)
	_ = DelayedVotingAdd(&items, &sum, base+interval, 10)
	_ = DelayedVotingAdd(&items, &sum, base+2*interval, 20)
	if len(items) != 3 || sum != 35 {
		t.Fatalf("setup: len=%d sum=%d", len(items), sum)
	}

	if err := DelayedVotingErase(&items, &sum, 3); err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || sum != 32 {
		t.Fatalf("erase 3: len=%d sum=%d", len(items), sum)
	}
	cmpDV(t, items, 2, base+2*interval, 17)

	if err := DelayedVotingErase(&items, &sum, 17); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || sum != 15 {
		t.Fatalf("erase 17: len=%d sum=%d", len(items), sum)
	}

	if err := DelayedVotingErase(&items, &sum, 12); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || sum != 3 {
		t.Fatalf("erase 12: len=%d sum=%d", len(items), sum)
	}
	cmpDV(t, items, 0, base, 3)

	if err := DelayedVotingErase(&items, &sum, 3); err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 || sum != 0 {
		t.Fatalf("erase final: len=%d sum=%d", len(items), sum)
	}

	if err := DelayedVotingErase(&items, &sum, 1); err == nil {
		t.Fatal("erase from empty should error")
	}
}
