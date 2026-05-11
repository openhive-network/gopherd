package parallel

import (
	"testing"

	"hived-go/internal/protocol/op"
)

func makeTouch(ops ...op.Operation) TouchSet {
	var t TouchSet
	for _, o := range ops {
		Extract(&t, o)
	}
	return t
}

func TestPlanBatches_AllDisjoint(t *testing.T) {
	touch := []TouchSet{
		makeTouch(op.AccountWitnessVote{Account: "a", Witness: "w1"}),
		makeTouch(op.AccountWitnessVote{Account: "b", Witness: "w2"}),
		makeTouch(op.AccountWitnessVote{Account: "c", Witness: "w3"}),
	}
	colors, numBatches := PlanBatches(touch)
	if numBatches != 1 {
		t.Fatalf("3 disjoint witness votes should batch into 1; got %d batches", numBatches)
	}
	for i, c := range colors {
		if c != 0 {
			t.Fatalf("colors[%d] = %d, want 0", i, c)
		}
	}
}

func TestPlanBatches_AllConflict(t *testing.T) {
	touch := []TouchSet{
		makeTouch(op.Vote{Voter: "a"}),
		makeTouch(op.Vote{Voter: "b"}),
		makeTouch(op.Vote{Voter: "c"}),
	}
	colors, numBatches := PlanBatches(touch)
	// Conservative: any two votes conflict via CommentTouched.
	if numBatches != 3 {
		t.Fatalf("3 conflicting votes should batch into 3; got %d", numBatches)
	}
	if colors[0] != 0 || colors[1] != 1 || colors[2] != 2 {
		t.Fatalf("colors = %v, want [0 1 2]", colors)
	}
}

func TestPlanBatches_MixedConflict(t *testing.T) {
	// tx0, tx1, tx2 are independent witness votes.
	// tx3 transfers from "a" — conflicts with tx0.
	// tx4 transfers from "d" — independent of all others.
	touch := []TouchSet{
		makeTouch(op.AccountWitnessVote{Account: "a", Witness: "w1"}),
		makeTouch(op.AccountWitnessVote{Account: "b", Witness: "w2"}),
		makeTouch(op.AccountWitnessVote{Account: "c", Witness: "w3"}),
		makeTouch(op.Transfer{From: "a", To: "x"}),
		makeTouch(op.Transfer{From: "d", To: "y"}),
	}
	colors, numBatches := PlanBatches(touch)
	// tx0..tx2 share batch 0 (disjoint witness votes).
	// tx3 conflicts with tx0 (both touch "a") → batch ≥ 1.
	// tx4 conflicts with tx3 (both FundsTouched) → batch ≥ 2.
	if numBatches < 2 {
		t.Fatalf("expected at least 2 batches; got %d, colors=%v", numBatches, colors)
	}
	// Verify within each batch, txs are pairwise non-conflicting.
	batches := GroupByBatch(colors, numBatches)
	for bi, batch := range batches {
		for i := 0; i < len(batch); i++ {
			for j := i + 1; j < len(batch); j++ {
				if touch[batch[i]].ConflictsWith(&touch[batch[j]]) {
					t.Fatalf("batch %d: txs %d and %d conflict", bi, batch[i], batch[j])
				}
			}
		}
	}
}

func TestPlanBatches_Empty(t *testing.T) {
	colors, numBatches := PlanBatches(nil)
	if numBatches != 0 || len(colors) != 0 {
		t.Fatalf("empty input: got numBatches=%d colors=%v", numBatches, colors)
	}
}

func TestGroupByBatch_PreservesInsertionOrder(t *testing.T) {
	colors := []int{0, 1, 0, 2, 1, 0}
	batches := GroupByBatch(colors, 3)
	expected := [][]int{{0, 2, 5}, {1, 4}, {3}}
	if len(batches) != len(expected) {
		t.Fatalf("len(batches) = %d, want %d", len(batches), len(expected))
	}
	for bi := range expected {
		if len(batches[bi]) != len(expected[bi]) {
			t.Fatalf("batch %d: len = %d, want %d", bi, len(batches[bi]), len(expected[bi]))
		}
		for i := range expected[bi] {
			if batches[bi][i] != expected[bi][i] {
				t.Fatalf("batch %d: order mismatch got=%v want=%v", bi, batches[bi], expected[bi])
			}
		}
	}
}
