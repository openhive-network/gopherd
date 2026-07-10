package sync

import (
	"testing"

	"hived-go/internal/protocol/block"
)

func mkID(b byte) block.ID { var id block.ID; id[0] = b; return id }

// TestForkDBReorgTopology builds a depth-2 tip fork and checks the fork DB's
// common-ancestor + branch extraction — the topology the reorg driver relies on.
//
//	num:   0        1        2        3
//	genesis(g) --- A1 --- A2            (incumbent head, height 2)
//	                 \___ B2 --- B3     (competing branch, height 3 → wins)
func TestForkDBReorgTopology(t *testing.T) {
	g, a1, a2, b2, b3 := mkID(0), mkID(1), mkID(2), mkID(3), mkID(4)
	f := newForkDB()
	f.add(g, block.ID{}, 0, nil)
	f.add(a1, g, 1, []byte("a1"))
	f.add(a2, a1, 2, []byte("a2"))
	f.setHead(a2)

	// Incumbent is the longest tip so far.
	if tip := f.longestKnownTip(); tip.id != a2 {
		t.Fatalf("longestKnownTip=%v want a2", tip.id[0])
	}

	// Competing branch arrives.
	f.add(b2, a1, 2, []byte("b2"))
	f.add(b3, b2, 3, []byte("b3"))

	// b3 (height 3) is now the longest known tip → the driver should reorg to it.
	if tip := f.longestKnownTip(); tip.id != b3 {
		t.Fatalf("longestKnownTip=%v want b3", tip.id[0])
	}

	// Common ancestor of the incumbent head (a2) and the winner (b3) is a1.
	anc, ok := f.commonAncestor(a2, b3)
	if !ok || anc != a1 {
		t.Fatalf("commonAncestor(a2,b3)=%v ok=%v want a1", anc[0], ok)
	}

	// The branch to undo: a1 → a2  (just a2).
	undo, ok := f.branchFrom(a1, a2)
	if !ok || len(undo) != 1 || undo[0].id != a2 {
		t.Fatalf("branchFrom(a1,a2)=%v ok=%v want [a2]", ids(undo), ok)
	}
	// The branch to apply: a1 → b2 → b3, in ascending order.
	apply, ok := f.branchFrom(a1, b3)
	if !ok || len(apply) != 2 || apply[0].id != b2 || apply[1].id != b3 {
		t.Fatalf("branchFrom(a1,b3)=%v ok=%v want [b2,b3]", ids(apply), ok)
	}
	// Raw bytes survive for re-apply.
	if string(apply[0].raw) != "b2" || string(apply[1].raw) != "b3" {
		t.Fatalf("branch raw mismatch: %q %q", apply[0].raw, apply[1].raw)
	}

	// A tip whose parent chain is not fully present is reported disconnected.
	orphan := mkID(9)
	f.add(orphan, mkID(8) /* unknown parent */, 5, []byte("orphan"))
	if _, ok := f.branchFrom(a1, orphan); ok {
		t.Fatal("branchFrom to a disconnected orphan should be ok=false")
	}

	// Prune drops the irreversible tail.
	f.prune(2)
	if _, ok := f.get(a1); ok {
		t.Fatal("prune(2) should drop height-1 a1")
	}
	if _, ok := f.get(b3); !ok {
		t.Fatal("prune(2) must keep height-3 b3")
	}
}

func ids(ns []*forkNode) []byte {
	out := make([]byte, len(ns))
	for i, n := range ns {
		out[i] = n.id[0]
	}
	return out
}
