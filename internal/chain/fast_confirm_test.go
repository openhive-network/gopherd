package chain

import (
	"encoding/binary"
	"fmt"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/block"
)

func fcTestID(num uint32, fork byte) block.ID {
	var id block.ID
	binary.BigEndian.PutUint32(id[:4], num)
	id[4] = fork
	return id
}

// fcTestChain applies blocks 1..head, produced round-robin by w0..w20.
func fcTestChain(head uint32) (*Database, []auth.AccountName) {
	d := &Database{}
	sched := make([]auth.AccountName, 21)
	for i := range sched {
		sched[i] = auth.AccountName(fmt.Sprintf("w%d", i))
	}
	for n := uint32(1); n <= head; n++ {
		d.noteAppliedForFastConfirm(n, fcTestID(n, 0), string(sched[n%21]))
	}
	return d, sched
}

// TestFastConfirmLIBNeedsSixteenOfTwentyOne: with production alone the LIB is
// the block 16 distinct producers have built on; once 16 witnesses vote for
// the head, approving or producing it, the head itself is irreversible.
func TestFastConfirmLIBNeedsSixteenOfTwentyOne(t *testing.T) {
	const head = 100
	d, sched := fcTestChain(head)

	// Production only: the 16th newest distinct producer's block.
	if got, want := d.fastConfirmLIB(sched, head, 0), uint32(head-15); got != want {
		t.Fatalf("production-only LIB=%d want %d", got, want)
	}

	d.fcApproved = map[string]block.ID{}
	// The head's producer (w16) votes for it by building it.
	for i := 0; i < 14; i++ {
		d.fcApproved[string(sched[i])] = fcTestID(head, 0)
	}
	if got := d.fastConfirmLIB(sched, head, 0); got == head {
		t.Fatalf("14 approvals plus the producer already made the head irreversible")
	}
	d.fcApproved[string(sched[14])] = fcTestID(head, 0)
	if got := d.fastConfirmLIB(sched, head, 0); got != head {
		t.Fatalf("15 approvals plus the producer: LIB=%d want %d", got, head)
	}
	// Nothing new above the current LIB.
	if got := d.fastConfirmLIB(sched, head, head); got != 0 {
		t.Fatalf("LIB already at head: got %d want 0", got)
	}
}

// TestFastConfirmLIBIgnoresOtherForks: approvals of a block that is not the
// one on our branch at that height must not count.
func TestFastConfirmLIBIgnoresOtherForks(t *testing.T) {
	const head = 100
	d, sched := fcTestChain(head)
	d.fcApproved = map[string]block.ID{}
	for i := 0; i < 21; i++ {
		d.fcApproved[string(sched[i])] = fcTestID(head, 1) // rival block at head
	}
	if got, want := d.fastConfirmLIB(sched, head, 0), uint32(head-15); got != want {
		t.Fatalf("approvals of a rival block moved LIB: got %d want %d", got, want)
	}
	// Approvals above our head (block not applied yet) wait for it.
	for i := 0; i < 21; i++ {
		d.fcApproved[string(sched[i])] = fcTestID(head+1, 0)
	}
	if got, want := d.fastConfirmLIB(sched, head, 0), uint32(head-15); got != want {
		t.Fatalf("approvals of an unapplied block moved LIB: got %d want %d", got, want)
	}
}

func TestDropSettledApprovals(t *testing.T) {
	d := &Database{fcApproved: map[string]block.ID{
		"a": fcTestID(10, 0), "b": fcTestID(11, 0), "c": fcTestID(12, 0),
	}}
	d.dropSettledApprovals(11)
	if _, ok := d.fcApproved["c"]; len(d.fcApproved) != 1 || !ok {
		t.Fatalf("kept %v, want only c", d.fcApproved)
	}
}
