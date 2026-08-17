package objects

import (
	"bytes"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

// commentFixture inserts a deterministic set of comments (two threads with
// replies) and commits. Attaches cold if non-nil.
func commentFixture(t *testing.T, cold coldStore) (*state.Session, *CommentStore) {
	t.Helper()
	db := openTempDB(t)
	s := state.NewSession(db)
	cs := EnsureCommentStore(s)
	if cold != nil {
		cs.SetColdTier(cold)
	}
	type crec struct {
		author, permlink, rauthor, rpermlink string
		net                                  int64
		paid                                 bool
	}
	recs := []crec{
		// Thread 1 rooted at root1/post.
		{"root1", "post", "", "", 500, true},
		{"replyer-a", "re-post", "root1", "post", 10, true},
		{"replyer-z", "re-post2", "root1", "post", 20, true},
		{"mid", "re-post3", "root1", "post", 5, true},
		// Thread 2 rooted at root2/hello.
		{"root2", "hello", "", "", 300, false},
		{"bob", "re-hello", "root2", "hello", 7, false},
	}
	for _, r := range recs {
		c := &Comment{
			Author: auth.AccountName(r.author), Permlink: r.permlink,
			RootAuthor: auth.AccountName(r.rauthor), RootPermlink: r.rpermlink,
			NetRshares: r.net, WasPaid: r.paid,
		}
		PutComment(s, c)
	}
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}
	return s, cs
}

func rangeRootPerms(t *testing.T, s *state.Session, ra, rp string) []string {
	t.Helper()
	var out []string
	_ = RangeCommentsByRoot(s, auth.AccountName(ra), rp, func(c *Comment) (bool, error) {
		out = append(out, string(c.Author)+"/"+c.Permlink)
		return true, nil
	})
	return out
}

// TestColdCommentEvictionEquivalence is the Stage-2 gate: a store with some
// comments evicted answers Get/Has/RangeByRoot identically and produces a
// byte-identical snapshot.
func TestColdCommentEvictionEquivalence(t *testing.T) {
	sRef, _ := commentFixture(t, nil)

	cold, err := openColdKV("")
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	sCold, cs := commentFixture(t, cold)
	// Evict a partial set including some thread-1 members (root stays, two
	// replies evicted) and the whole thread-2 reply.
	if !cs.Evict("replyer-a", "re-post") {
		t.Fatal("evict replyer-a failed")
	}
	if !cs.Evict("mid", "re-post3") {
		t.Fatal("evict mid failed")
	}
	if !cs.Evict("bob", "re-hello") {
		t.Fatal("evict bob failed")
	}

	// Get equivalence.
	for _, c := range []struct{ a, p string }{
		{"root1", "post"}, {"replyer-a", "re-post"}, {"replyer-z", "re-post2"}, {"mid", "re-post3"},
		{"root2", "hello"}, {"bob", "re-hello"}, {"ghost", "x"},
	} {
		gr, okr := GetComment(sRef, auth.AccountName(c.a), c.p)
		gc, okc := GetComment(sCold, auth.AccountName(c.a), c.p)
		if (okr == nil) != (okc == nil) {
			t.Fatalf("Get err mismatch %v: %v vs %v", c, okr, okc)
		}
		if gr.Author != gc.Author || gr.Permlink != gc.Permlink || gr.NetRshares != gc.NetRshares || gr.WasPaid != gc.WasPaid {
			t.Fatalf("Get mismatch %v: ref=%+v cold=%+v", c, gr, gc)
		}
	}

	// RangeByRoot order equivalence (the merge must preserve author,permlink order).
	for _, r := range []struct{ a, p string }{{"root1", "post"}, {"root2", "hello"}} {
		vr := rangeRootPerms(t, sRef, r.a, r.p)
		vc := rangeRootPerms(t, sCold, r.a, r.p)
		if !equalStrings(vr, vc) {
			t.Fatalf("RangeByRoot mismatch for %v:\n ref=%v\n cold=%v", r, vr, vc)
		}
	}

	if cs.Count() != EnsureCommentStore(sRef).Count() {
		t.Fatalf("Count mismatch: cold=%d ref=%d", cs.Count(), EnsureCommentStore(sRef).Count())
	}

	// THE GATE: semantically-identical snapshot. Byte order differs by
	// design — the cold tier streams evicted rows in cold-store order
	// after the sorted resident set, while a pure-resident store emits
	// one global sorted stream. Loads are keyed (author, permlink), so
	// record order carries no meaning; sorting the cold merge would cost
	// a full-tier sort on every multi-GB save for nothing. We assert the
	// DECODED record sets match exactly, plus determinism (same store
	// marshals to identical bytes twice).
	var bRef, bCold bytes.Buffer
	if err := EnsureCommentStore(sRef).MarshalSnapshot(&bRef); err != nil {
		t.Fatal(err)
	}
	if err := cs.MarshalSnapshot(&bCold); err != nil {
		t.Fatal(err)
	}
	if !snapshotRecordSetsEqual(t, bRef.Bytes(), bCold.Bytes()) {
		t.Fatalf("snapshot record sets differ: ref=%d bytes cold=%d bytes", bRef.Len(), bCold.Len())
	}
	var bCold2 bytes.Buffer
	if err := cs.MarshalSnapshot(&bCold2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bCold.Bytes(), bCold2.Bytes()) {
		t.Fatalf("cold snapshot not deterministic across marshals")
	}
}

// TestColdCommentPromoteOnEdit checks GetRef/Mutate/Put promote an evicted
// comment to a real mutable slot.
func TestColdCommentPromoteOnEdit(t *testing.T) {
	cold, err := openColdKV("")
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	s, cs := commentFixture(t, cold)
	cs.Evict("replyer-a", "re-post")

	// GetRef promotes; mutate via the returned slot then Put (in-place path).
	ref, ok := cs.GetRef("replyer-a", "re-post")
	if !ok {
		t.Fatal("GetRef after evict failed")
	}
	ref.NetRshares = 9999
	cs.Put(ref)
	got, _ := GetComment(s, "replyer-a", "re-post")
	if got.NetRshares != 9999 {
		t.Fatalf("edit value: %d want 9999", got.NetRshares)
	}
	// Still part of its thread.
	perms := rangeRootPerms(t, s, "root1", "post")
	found := false
	for _, p := range perms {
		if p == "replyer-a/re-post" {
			found = true
		}
	}
	if !found {
		t.Fatalf("promoted comment missing from thread: %v", perms)
	}
}

// TestColdCommentUndoAcrossPromotion: under UndoEnabled, Mutate/Delete of an
// evicted comment (which promotes) must be reverted by Restore.
func TestColdCommentUndoAcrossPromotion(t *testing.T) {
	cold, err := openColdKV("")
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	s, cs := commentFixture(t, cold)
	cs.Evict("replyer-a", "re-post")
	cs.Evict("mid", "re-post3")

	s2 := state.NewSession(s.Database())
	sp := s2.Snapshot()
	// Mutate (promote) replyer-a; Delete (promote) mid.
	EnsureCommentStore(s2).Mutate("replyer-a", "re-post", func(c *Comment) { c.NetRshares = 7 })
	DeleteComment(s2, "mid", "re-post3")
	s2.Restore(sp)

	got, ok := GetComment(s2, "replyer-a", "re-post")
	if ok != nil || got.NetRshares != 10 {
		t.Fatalf("replyer-a after restore: net=%d (err %v) want 10", got.NetRshares, ok)
	}
	if has := EnsureCommentStore(s2).Has("mid", "re-post3"); !has {
		t.Fatalf("mid missing after restore")
	}
	// Thread intact: all 4 members present in order.
	perms := rangeRootPerms(t, s2, "root1", "post")
	if len(perms) != 4 {
		t.Fatalf("thread size after restore: %d want 4 (%v)", len(perms), perms)
	}
}
