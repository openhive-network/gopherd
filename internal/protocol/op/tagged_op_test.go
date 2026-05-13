package op

import (
	"testing"
	"unsafe"
)

// TestOpFromOperationRoundtrip verifies the FromOperation / Operation /
// AsX trio agrees with the original Operation iface for a representative
// op (Vote).
func TestOpFromOperationRoundtrip(t *testing.T) {
	orig := Vote{Voter: "alice", Author: "bob", Permlink: "x", Weight: 10000}
	wrapped := FromOperation(orig)

	if wrapped.Tag != TagVote {
		t.Fatalf("Tag: got %d want %d", wrapped.Tag, TagVote)
	}
	if wrapped.ptr == nil {
		t.Fatal("ptr is nil after FromOperation")
	}

	// AsVote returns a typed pointer to the concrete data.
	got := wrapped.AsVote()
	if got == nil {
		t.Fatal("AsVote returned nil")
	}
	if got.Voter != "alice" || got.Author != "bob" || got.Permlink != "x" || got.Weight != 10000 {
		t.Fatalf("AsVote mismatch: %+v", *got)
	}

	// Round-trip back to Operation iface.
	back := wrapped.Operation()
	if back == nil {
		t.Fatal("Operation() returned nil")
	}
	if back.OpTag() != TagVote {
		t.Fatalf("Operation().OpTag(): got %d want %d", back.OpTag(), TagVote)
	}
	bv, ok := back.(Vote)
	if !ok {
		t.Fatalf("Operation() returned %T, want Vote", back)
	}
	if bv != orig {
		t.Fatalf("Operation round-trip mismatch:\n got: %+v\n want: %+v", bv, orig)
	}
}

// TestOpFromPtr verifies constructing an Op directly from a typed
// pointer (the path readers will use after migration).
func TestOpFromPtr(t *testing.T) {
	concrete := &Transfer{From: "alice", To: "bob", Memo: "hi"}
	wrapped := OpFromPtr(TagTransfer, unsafe.Pointer(concrete))

	if wrapped.Tag != TagTransfer {
		t.Fatalf("Tag: got %d want %d", wrapped.Tag, TagTransfer)
	}
	got := wrapped.AsTransfer()
	if got != concrete {
		t.Fatalf("AsTransfer returned a different pointer than the source")
	}
	if got.From != "alice" || got.To != "bob" || got.Memo != "hi" {
		t.Fatalf("AsTransfer field mismatch: %+v", *got)
	}
}

// TestOpZeroValue confirms a zero Op behaves sanely.
func TestOpZeroValue(t *testing.T) {
	var z Op
	if z.Tag != 0 || z.ptr != nil {
		t.Fatalf("zero Op: %+v", z)
	}
	if z.Operation() != nil {
		t.Fatal("Operation() on zero Op should return nil")
	}
}
