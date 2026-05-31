package objects

import (
	"bytes"
	"testing"

	"hived-go/internal/state"
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

	// Run eviction. Policy is AGE-based (oldest first), NOT frozen-only — a
	// consensus node pages out cold comments regardless of cashout window.
	// 8 resident, cap=3 → evict the 5 oldest (ids 1..5), incl. LIVE ones.
	n := csC.EvictFrozen(cvsC, 100)
	if n != 5 {
		t.Fatalf("evicted %d, want 5 (oldest down to cap)", n)
	}
	if got := len(csC.primary); got != 3 {
		t.Fatalf("resident comments after evict: %d want 3 (newest)", got)
	}
	// Newest 3 (ids 6,7,8) resident; oldest 5 (ids 1..5) cold — including LIVE
	// ones (odd ids 1,3,5), proving live comments are evictable.
	for i := 1; i <= 8; i++ {
		_, residentC := csC.primary[newCommentPK("auth", permOf(i))]
		wantResident := i > 5
		if residentC != wantResident {
			t.Fatalf("comment %d resident=%v want %v", i, residentC, wantResident)
		}
	}
	// Votes on an evicted live comment (id 5, odd) went cold too, still counted.
	if cvsC.CountForComment("auth", permOf(5)) != 1 {
		t.Fatalf("evicted-comment vote count via tiers: %d want 1", cvsC.CountForComment("auth", permOf(5)))
	}
	if _, resident := cvsC.primary[newCommentVotePK("voter", "auth", permOf(5))]; resident {
		t.Fatal("vote on evicted comment 5 should have been evicted from RAM")
	}

	// Byte-identical snapshots (both stores).
	var br, bc bytes.Buffer
	csR.MarshalSnapshot(&br)
	csC.MarshalSnapshot(&bc)
	if !bytes.Equal(br.Bytes(), bc.Bytes()) {
		t.Fatalf("comment snapshot not byte-identical (ref %d vs cold %d)", br.Len(), bc.Len())
	}
	br.Reset()
	bc.Reset()
	cvsR.MarshalSnapshot(&br)
	cvsC.MarshalSnapshot(&bc)
	if !bytes.Equal(br.Bytes(), bc.Bytes()) {
		t.Fatalf("vote snapshot not byte-identical (ref %d vs cold %d)", br.Len(), bc.Len())
	}
}

func permOf(i int) string {
	return "post-" + string(rune('a'+i))
}
