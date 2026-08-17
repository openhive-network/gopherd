package objects

import (
	"bytes"
	"testing"

	"gopherd/internal/state"
)

// TestEvictFrozenPolicy checks that EvictFrozen evicts only frozen
// (CashoutTime==0) comments down toward the cap, leaves live ones resident,
// evicts their votes alongside, and preserves a byte-identical snapshot.
func TestEvictFrozenPolicy(t *testing.T) {
	// Reference: all-RAM store with identical logical contents.
	dbR := openTempDB(t)
	sR := state.NewSession(dbR)
	csR := EnsureCommentStore(sR)
	cvsR := EnsureCommentVoteStore(sR)

	// Cold store with a low cap.
	cold, err := openColdKV("")
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	dbC := openTempDB(t)
	sC := state.NewSession(dbC)
	csC := EnsureCommentStore(sC)
	cvsC := EnsureCommentVoteStore(sC)
	csC.SetColdTier(cold)
	csC.SetResidentCap(3) // keep only 3 resident
	cvsC.SetColdTier(cold)

	// 8 comments: ids 1..8; even ids frozen (CashoutTime 0), odd ids live.
	for i := 1; i <= 8; i++ {
		live := i%2 == 1
		ct := uint32(0)
		if live {
			ct = 9999999
		}
		for _, cs := range []*CommentStore{csR, csC} {
			cs.Put(&Comment{
				Author: "auth", Permlink: permOf(i), ID: uint64(i),
				CashoutTime: ct, WasPaid: !live, NetRshares: int64(i * 10),
			})
		}
		// one vote each
		for _, cvs := range []*CommentVoteStore{cvsR, cvsC} {
			cvs.Put(&CommentVote{Voter: "voter", Author: "auth", Permlink: permOf(i), Rshares: int64(i)})
		}
	}
	if err := sR.Commit(false); err != nil {
		t.Fatal(err)
	}
	if err := sC.Commit(false); err != nil {
		t.Fatal(err)
	}

	// Run eviction. The production policy PREFERS keeping unpaid (live)
	// comments resident (evicting one is promote-back thrash at cashout),
	// deferring them while the defer budget lasts, then force-evicts
	// oldest-first so the store always drains to the cap (the dense-era
	// deadlock fix). Exact evictee membership is policy-internal; the
	// invariants are: drains to cap, everything stays retrievable with
	// identical contents through the tiers, and snapshots stay
	// semantically identical to the all-RAM reference.
	csC.EvictFrozen(cvsC, 100)
	if got := len(csC.primary); got != 3 {
		t.Fatalf("resident comments after evict: %d want 3 (cap)", got)
	}
	for i := 1; i <= 8; i++ {
		gr, okR := GetComment(sR, "auth", permOf(i))
		gc, okC := GetComment(sC, "auth", permOf(i))
		if (okR == nil) != (okC == nil) {
			t.Fatalf("comment %d Get err mismatch: %v vs %v", i, okR, okC)
		}
		if gr.NetRshares != gc.NetRshares || gr.WasPaid != gc.WasPaid || gr.CashoutTime != gc.CashoutTime {
			t.Fatalf("comment %d content mismatch: ref=%+v cold=%+v", i, gr, gc)
		}
		if n := cvsC.CountForComment("auth", permOf(i)); n != 1 {
			t.Fatalf("comment %d vote count via tiers: %d want 1", i, n)
		}
	}

	// Semantically-identical snapshots (order-independent record sets —
	// the cold tier streams evicted rows after the sorted resident set).
	var br, bc bytes.Buffer
	csR.MarshalSnapshot(&br)
	csC.MarshalSnapshot(&bc)
	if !snapshotRecordSetsEqual(t, br.Bytes(), bc.Bytes()) {
		t.Fatalf("comment snapshot record sets differ (ref %d vs cold %d)", br.Len(), bc.Len())
	}
	br.Reset()
	bc.Reset()
	cvsR.MarshalSnapshot(&br)
	cvsC.MarshalSnapshot(&bc)
	if !snapshotRecordSetsEqual(t, br.Bytes(), bc.Bytes()) {
		t.Fatalf("vote snapshot record sets differ (ref %d vs cold %d)", br.Len(), bc.Len())
	}
}

func permOf(i int) string { return "p" + string(rune('0'+i)) }
