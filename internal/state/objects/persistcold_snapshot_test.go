package objects

import (
	"bytes"
	"path/filepath"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

// TestDurableColdSnapshotRestart is the end-to-end cutover proof WITHOUT a
// replay: a CommentStore with a PERSISTENT cold tier (PersistCold) evicts some
// comments to cold, saves a snapshot (which now omits the cold rows), then a
// fresh store reopens the SAME cold dir and loads that snapshot — and answers
// Get/RangeByRoot for BOTH resident and cold comments. This is "restart at head
// with no replay": the resident set comes from the small snapshot, the bulk
// (cold) comes from the remapped persistent cold tier.
func TestDurableColdSnapshotRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cold")

	cold1, err := OpenPersistCold(dir)
	if err != nil {
		t.Fatal(err)
	}
	sA, csA := commentFixture(t, cold1)

	// Evict a partial set to the (durable) cold tier — some replies + a root.
	for _, e := range []struct{ a, p string }{
		{"replyer-a", "re-post"}, {"mid", "re-post3"}, {"bob", "re-hello"}, {"root2", "hello"},
	} {
		if !csA.Evict(e.a, e.p) {
			t.Fatalf("evict %s/%s failed", e.a, e.p)
		}
	}
	wantCount := csA.Count()
	if csA.coldTotal == 0 {
		t.Fatal("expected some comments in the cold tier")
	}

	// Capture every comment's identity for the post-restart comparison.
	all := []struct{ a, p string }{
		{"root1", "post"}, {"replyer-a", "re-post"}, {"replyer-z", "re-post2"},
		{"mid", "re-post3"}, {"root2", "hello"}, {"bob", "re-hello"},
	}
	want := map[string]Comment{}
	for _, c := range all {
		got, err := GetComment(sA, auth.AccountName(c.a), c.p)
		if err != nil {
			t.Fatalf("pre-save get %s/%s: %v", c.a, c.p, err)
		}
		want[c.a+"/"+c.p] = got
	}
	rootsPre := map[string][]string{
		"root1/post":  rangeRootPerms(t, sA, "root1", "post"),
		"root2/hello": rangeRootPerms(t, sA, "root2", "hello"),
	}

	// Save the snapshot (resident-only, cold omitted) + clean-shutdown the cold tier.
	var buf bytes.Buffer
	if err := csA.MarshalSnapshot(&buf); err != nil {
		t.Fatalf("MarshalSnapshot: %v", err)
	}
	if err := cold1.Close(); err != nil {
		t.Fatalf("cold close: %v", err)
	}
	t.Logf("snapshot is resident-only: %d bytes for %d total comments (%d cold omitted)",
		buf.Len(), wantCount, csA.coldTotal)

	// === RESTART ===: reopen the SAME cold dir, fresh store, load the snapshot.
	cold2, err := OpenPersistCold(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cold2.Close()
	db := openTempDB(t)
	sB := state.NewSession(db)
	csB := EnsureCommentStore(sB)
	csB.SetColdTier(cold2)
	if err := csB.UnmarshalSnapshot(&buf); err != nil {
		t.Fatalf("UnmarshalSnapshot: %v", err)
	}

	if csB.Count() != wantCount {
		t.Fatalf("restart count %d want %d (coldTotal=%d)", csB.Count(), wantCount, csB.coldTotal)
	}
	for _, c := range all {
		got, err := GetComment(sB, auth.AccountName(c.a), c.p)
		if err != nil {
			t.Fatalf("restart get %s/%s: %v (cold rows lost?)", c.a, c.p, err)
		}
		w := want[c.a+"/"+c.p]
		if got.Author != w.Author || got.Permlink != w.Permlink || got.NetRshares != w.NetRshares || got.WasPaid != w.WasPaid {
			t.Fatalf("restart mismatch %s/%s: got=%+v want=%+v", c.a, c.p, got, w)
		}
	}
	// RangeByRoot still merges resident + cold correctly after restart.
	for rk, wantPerms := range rootsPre {
		var ra, rp string
		switch rk {
		case "root1/post":
			ra, rp = "root1", "post"
		case "root2/hello":
			ra, rp = "root2", "hello"
		}
		got := rangeRootPerms(t, sB, ra, rp)
		if !equalStrings(got, wantPerms) {
			t.Fatalf("restart RangeByRoot %s: got=%v want=%v", rk, got, wantPerms)
		}
	}
	t.Logf("RESTART AT HEAD ok: %d comments (resident + durable cold) intact after snapshot reload — NO REPLAY", wantCount)
}

// TestDurableColdVoteSnapshotRestart is the same cutover proof for the
// CommentVoteStore (votes are the largest cold tier of all).
func TestDurableColdVoteSnapshotRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "voteCold")
	cold1, err := OpenPersistCold(dir)
	if err != nil {
		t.Fatal(err)
	}
	sA, cvsA := voteFixture(t, cold1)
	if n := cvsA.EvictComment("auth1", "post-a"); n != 3 {
		t.Fatalf("evicted %d from post-a, want 3", n)
	}
	if n := cvsA.EvictComment("auth2", "post-c"); n != 2 {
		t.Fatalf("evicted %d from post-c, want 2", n)
	}

	all := []struct{ v, a, p string }{
		{"alice", "auth1", "post-a"}, {"bob", "auth1", "post-a"}, {"carol", "auth1", "post-a"},
		{"alice", "auth1", "post-b"}, {"dave", "auth1", "post-b"},
		{"bob", "auth2", "post-c"}, {"zeb", "auth2", "post-c"},
	}
	want := map[string]CommentVote{}
	for _, c := range all {
		got, err := GetCommentVote(sA, auth.AccountName(c.v), auth.AccountName(c.a), c.p)
		if err != nil {
			t.Fatalf("pre-save get %v: %v", c, err)
		}
		want[c.v+"|"+c.a+"|"+c.p] = got
	}
	wantA := cvsA.CountForComment("auth1", "post-a")
	wantC := cvsA.CountForComment("auth2", "post-c")

	var buf bytes.Buffer
	if err := cvsA.MarshalSnapshot(&buf); err != nil {
		t.Fatalf("MarshalSnapshot: %v", err)
	}
	if err := cold1.Close(); err != nil {
		t.Fatal(err)
	}

	// === RESTART ===
	cold2, err := OpenPersistCold(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cold2.Close()
	db := openTempDB(t)
	sB := state.NewSession(db)
	cvsB := EnsureCommentVoteStore(sB)
	cvsB.SetColdTier(cold2)
	if err := cvsB.UnmarshalSnapshot(&buf); err != nil {
		t.Fatalf("UnmarshalSnapshot: %v", err)
	}

	if got := cvsB.CountForComment("auth1", "post-a"); got != wantA {
		t.Fatalf("restart count post-a %d want %d (cold votes lost?)", got, wantA)
	}
	if got := cvsB.CountForComment("auth2", "post-c"); got != wantC {
		t.Fatalf("restart count post-c %d want %d", got, wantC)
	}
	for _, c := range all {
		got, err := GetCommentVote(sB, auth.AccountName(c.v), auth.AccountName(c.a), c.p)
		if err != nil {
			t.Fatalf("restart get %v: %v (cold vote lost?)", c, err)
		}
		if got != want[c.v+"|"+c.a+"|"+c.p] {
			t.Fatalf("restart vote mismatch %v: got=%+v want=%+v", c, got, want[c.v+"|"+c.a+"|"+c.p])
		}
	}
	t.Logf("RESTART AT HEAD ok: votes (resident + durable cold) intact via Get + per-comment count — NO REPLAY")
}
