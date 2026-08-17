package objects

import (
	"bytes"
	"sort"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

// voteFixture inserts a deterministic set of votes across several comments
// and commits, returning the store. If cold != nil it is attached first.
func voteFixture(t *testing.T, cold coldStore) (*state.Session, *CommentVoteStore) {
	t.Helper()
	db := openTempDB(t)
	s := state.NewSession(db)
	cvs := EnsureCommentVoteStore(s)
	if cold != nil {
		cvs.SetColdTier(cold)
	}
	// 3 comments, several voters each, varied fields.
	type vrec struct {
		voter, author, permlink string
		rshares                 int64
		cw                      uint64
	}
	recs := []vrec{
		{"alice", "auth1", "post-a", 100, 11},
		{"bob", "auth1", "post-a", -50, 0},
		{"carol", "auth1", "post-a", 7, 3},
		{"alice", "auth1", "post-b", 200, 22},
		{"dave", "auth1", "post-b", 9, 1},
		{"bob", "auth2", "post-c", 5, 5},
		{"zeb", "auth2", "post-c", 1, 0},
	}
	for _, r := range recs {
		PutCommentVote(s, &CommentVote{
			Voter: auth.AccountName(r.voter), Author: auth.AccountName(r.author),
			Permlink: r.permlink, Rshares: r.rshares, CurationWeight: r.cw,
			Weight: int16(r.rshares % 1000), LastUpdate: 1000 + uint32(r.rshares),
		})
	}
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}
	return s, cvs
}

func sortedVotersOn(t *testing.T, s *state.Session, author, permlink string) []string {
	t.Helper()
	var out []string
	_ = RangeCommentVotesByComment(s, auth.AccountName(author), permlink, func(cv *CommentVote) (bool, error) {
		out = append(out, string(cv.Voter))
		return true, nil
	})
	sort.Strings(out)
	return out
}

// TestColdVoteEvictionEquivalence is the Stage-1 gate: a store with some
// comments' votes evicted to the cold tier must answer Get/Has/RangeByComment/
// Count identically to an all-RAM store, and MUST produce a byte-identical
// snapshot.
func TestColdVoteEvictionEquivalence(t *testing.T) {
	// All-RAM reference.
	sRef, _ := voteFixture(t, nil)

	// Cold-backed store with the same contents, then evict post-a and post-c.
	cold, err := openColdKV("")
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	sCold, cvs := voteFixture(t, cold)
	if n := cvs.EvictComment("auth1", "post-a"); n != 3 {
		t.Fatalf("evicted %d from post-a, want 3", n)
	}
	if n := cvs.EvictComment("auth2", "post-c"); n != 2 {
		t.Fatalf("evicted %d from post-c, want 2", n)
	}

	// Get equivalence (resident + evicted).
	for _, c := range []struct{ v, a, p string }{
		{"alice", "auth1", "post-a"}, {"bob", "auth1", "post-a"}, {"carol", "auth1", "post-a"},
		{"alice", "auth1", "post-b"}, {"dave", "auth1", "post-b"},
		{"bob", "auth2", "post-c"}, {"zeb", "auth2", "post-c"},
		{"nobody", "auth1", "post-a"}, // miss
	} {
		gr, okr := GetCommentVote(sRef, auth.AccountName(c.v), auth.AccountName(c.a), c.p)
		gc, okc := GetCommentVote(sCold, auth.AccountName(c.v), auth.AccountName(c.a), c.p)
		if (okr == nil) != (okc == nil) {
			t.Fatalf("Get err mismatch for %v: ref=%v cold=%v", c, okr, okc)
		}
		if gr != gc {
			t.Fatalf("Get value mismatch for %v: ref=%+v cold=%+v", c, gr, gc)
		}
	}

	// RangeByComment + Count equivalence.
	for _, c := range []struct{ a, p string }{{"auth1", "post-a"}, {"auth1", "post-b"}, {"auth2", "post-c"}} {
		vr := sortedVotersOn(t, sRef, c.a, c.p)
		vc := sortedVotersOn(t, sCold, c.a, c.p)
		if !equalStrings(vr, vc) {
			t.Fatalf("RangeByComment mismatch for %v: ref=%v cold=%v", c, vr, vc)
		}
		nr := EnsureCommentVoteStore(sRef).CountForComment(auth.AccountName(c.a), c.p)
		nc := cvs.CountForComment(auth.AccountName(c.a), c.p)
		if nr != nc {
			t.Fatalf("Count mismatch for %v: ref=%d cold=%d", c, nr, nc)
		}
	}

	// THE GATE: semantically-identical snapshot (order-independent record
	// sets; see the comment-store variant for rationale) + determinism.
	var bRef, bCold bytes.Buffer
	if err := EnsureCommentVoteStore(sRef).MarshalSnapshot(&bRef); err != nil {
		t.Fatal(err)
	}
	if err := cvs.MarshalSnapshot(&bCold); err != nil {
		t.Fatal(err)
	}
	if !snapshotRecordSetsEqual(t, bRef.Bytes(), bCold.Bytes()) {
		t.Fatalf("snapshot record sets differ: ref=%d bytes cold=%d bytes", bRef.Len(), bCold.Len())
	}
	var bCold2 bytes.Buffer
	if err := cvs.MarshalSnapshot(&bCold2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bCold.Bytes(), bCold2.Bytes()) {
		t.Fatalf("cold snapshot not deterministic across marshals")
	}
}

// TestColdVotePromoteOnRecast checks that a recast (Put) of an evicted vote
// promotes + overwrites correctly, and DeleteAllForComment clears cold votes.
func TestColdVotePromoteOnRecast(t *testing.T) {
	cold, err := openColdKV("")
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	s, cvs := voteFixture(t, cold)
	cvs.EvictComment("auth1", "post-a") // alice/bob/carol now cold

	// Recast alice's vote (was cold) — should promote + overwrite.
	PutCommentVote(s, &CommentVote{Voter: "alice", Author: "auth1", Permlink: "post-a", Rshares: 999})
	got, _ := GetCommentVote(s, "alice", "auth1", "post-a")
	if got.Rshares != 999 {
		t.Fatalf("recast value: %d want 999", got.Rshares)
	}
	if cvs.CountForComment("auth1", "post-a") != 3 {
		t.Fatalf("count after recast: %d want 3", cvs.CountForComment("auth1", "post-a"))
	}

	// DeleteAllForComment must clear both tiers (carol/bob still cold, alice hot).
	DeleteAllCommentVotesForComment(s, "auth1", "post-a")
	if cvs.CountForComment("auth1", "post-a") != 0 {
		t.Fatalf("count after DeleteAll: %d want 0", cvs.CountForComment("auth1", "post-a"))
	}
	for _, v := range []string{"alice", "bob", "carol"} {
		if has, _ := HasCommentVote(s, auth.AccountName(v), "auth1", "post-a"); has {
			t.Fatalf("vote %s survived DeleteAll", v)
		}
	}
}

// TestColdVoteUndoAcrossPromotion is the critical correctness test: under
// UndoEnabled, mutating/deleting an evicted vote (which promotes it) must be
// fully reverted by Restore.
func TestColdVoteUndoAcrossPromotion(t *testing.T) {
	cold, err := openColdKV("")
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	s, cvs := voteFixture(t, cold)
	cvs.EvictComment("auth1", "post-a") // alice/bob/carol cold

	s2 := state.NewSession(s.Database())
	sp := s2.Snapshot() // undo ON
	// Promote-and-mutate alice; promote-and-delete bob.
	PutCommentVote(s2, &CommentVote{Voter: "alice", Author: "auth1", Permlink: "post-a", Rshares: 12345})
	DeleteCommentVote(s2, "bob", "auth1", "post-a")
	s2.Restore(sp)

	// alice reverts to original rshares (100); bob still present.
	got, ok := GetCommentVote(s2, "alice", "auth1", "post-a")
	if ok != nil || got.Rshares != 100 {
		t.Fatalf("alice after restore: rshares=%d (err %v) want 100", got.Rshares, ok)
	}
	if has, _ := HasCommentVote(s2, "bob", "auth1", "post-a"); !has {
		t.Fatalf("bob missing after restore")
	}
	if cvs.CountForComment("auth1", "post-a") != 3 {
		t.Fatalf("count after restore: %d want 3", cvs.CountForComment("auth1", "post-a"))
	}
}
