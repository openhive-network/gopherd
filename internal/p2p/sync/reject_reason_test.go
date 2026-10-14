package sync

import (
	"errors"
	"testing"
)

func TestRejectReasonFoldsNumbersAndPrefix(t *testing.T) {
	got := rejectReason("witness.AddPendingRaw: rejected: transaction expired (HF9+): expiration=1760461200 head=1760461203")
	if want := "transaction expired (HFN+): expiration=N head=N"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	var d Delegate
	d.noteTxReject(errors.New("witness.AddPendingRaw: rejected: duplicate transaction (already in dedup window)"))
	d.noteTxReject(errors.New("witness.AddPendingRaw: rejected: duplicate transaction (already in dedup window)"))
	d.noteTxReject(errors.New("witness.AddPendingRaw: rejected: tapos: ref_block_num 12 not present"))
	r := d.TxRejectReasons(5)
	if len(r) != 2 || r[0] != `"duplicate transaction (already in dedup window)"=2(67%)` {
		t.Fatalf("reasons: %v", r)
	}
}
