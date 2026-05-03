package objects

import "testing"

// TestDelayedVotingProcessor03 ports delayed_voting_tests.cpp::
// delayed_voting_processor_03. Exercises the no-op edges:
//   - erase(0) on empty queue is fine
//   - add(val=0) is fine (and doesn't append)
//   - erase_front on empty queue is fine
//   - erase(1) on empty queue must FAIL (not enough votes)
//   - add(val) then erase(val) leaves an empty queue
//   - erase across multiple buckets consumes from the end first
func TestDelayedVotingProcessor03(t *testing.T) {
	var dq []DelayedVoteEntry
	var sum uint64
	const time uint32 = 1580611445 // 2020-02-02T03:04:05 UTC

	// nothing-to-do edges.
	if err := DelayedVotingErase(&dq, &sum, 0); err != nil {
		t.Fatalf("erase(0) on empty: %v", err)
	}
	if err := DelayedVotingAdd(&dq, &sum, time, 0); err != nil {
		t.Fatalf("add(val=0): %v", err)
	}
	if err := DelayedVotingEraseFront(&dq, &sum); err != nil {
		t.Fatalf("erase_front on empty: %v", err)
	}
	if len(dq) != 0 {
		t.Fatalf("after no-ops: len=%d, want 0", len(dq))
	}

	// erase(1) on empty must FAIL.
	if err := DelayedVotingErase(&dq, &sum, 1); err == nil {
		t.Fatal("erase(1) on empty queue should fail")
	}

	// add(666) then erase(666) returns to empty.
	if err := DelayedVotingAdd(&dq, &sum, time, 666); err != nil {
		t.Fatalf("add(666): %v", err)
	}
	if err := DelayedVotingErase(&dq, &sum, 666); err != nil {
		t.Fatalf("erase(666): %v", err)
	}
	if len(dq) != 0 {
		t.Fatalf("after add(666)+erase(666): len=%d, want 0", len(dq))
	}

	// add two on different days, erase the full sum (55) — empty queue.
	dayInSec := uint32(24 * 60 * 60)
	if err := DelayedVotingAdd(&dq, &sum, time+dayInSec, 22); err != nil {
		t.Fatalf("add 22: %v", err)
	}
	if err := DelayedVotingAdd(&dq, &sum, time+2*dayInSec, 33); err != nil {
		t.Fatalf("add 33: %v", err)
	}
	if err := DelayedVotingErase(&dq, &sum, 55); err != nil {
		t.Fatalf("erase 55: %v", err)
	}
	if len(dq) != 0 {
		t.Fatalf("after erase-all: len=%d, want 0", len(dq))
	}
}

// TestDelayedVotingEraseAcrossBuckets ports the "two-bucket erase from
// back" portion of delayed_voting_processor_02. Erasing partial votes
// shrinks the LAST bucket; once it reaches 0 the bucket is removed.
func TestDelayedVotingEraseAcrossBuckets(t *testing.T) {
	var dq []DelayedVoteEntry
	var sum uint64
	const base uint32 = 1577934245

	dayInSec := uint32(24 * 60 * 60)
	for i := uint32(1); i <= 10; i++ {
		if err := DelayedVotingAdd(&dq, &sum, base+i*dayInSec, uint64(i)); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if len(dq) != 10 {
		t.Fatalf("after 10 adds: len=%d, want 10", len(dq))
	}
	// sum = 1+2+...+10 = 55.
	if sum != 55 {
		t.Fatalf("sum: got %d, want 55", sum)
	}

	// Erase 1 vote → last bucket goes from 10 → 9.
	if err := DelayedVotingErase(&dq, &sum, 1); err != nil {
		t.Fatalf("erase 1: %v", err)
	}
	if dq[9].Val != 9 {
		t.Errorf("last bucket Val: got %d, want 9", dq[9].Val)
	}
	if sum != 54 {
		t.Errorf("sum after erase 1: got %d, want 54", sum)
	}

	// Erase 9 more → last bucket reaches 0, gets removed.
	if err := DelayedVotingErase(&dq, &sum, 9); err != nil {
		t.Fatalf("erase 9: %v", err)
	}
	if len(dq) != 9 {
		t.Errorf("after last bucket drained: len=%d, want 9", len(dq))
	}
	if sum != 45 {
		t.Errorf("sum: got %d, want 45", sum)
	}
}

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
