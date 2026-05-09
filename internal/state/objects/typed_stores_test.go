package objects

import (
	"sort"
	"testing"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// TestAccountStoreSnapshotRestore exercises the chainbase-style undo log
// on AccountStore: a Put after Snapshot is reverted by Restore, leaving
// the prior value visible.
func TestAccountStoreSnapshotRestore(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	PutAccount(s, &Account{Name: "alice", Balance: 100})
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}

	s2 := state.NewSession(db)
	sp := s2.Snapshot()
	PutAccount(s2, &Account{Name: "alice", Balance: 999})
	got, _ := GetAccount(s2, "alice")
	if got.Balance != 999 {
		t.Fatalf("after Put before Restore: balance=%d want 999", got.Balance)
	}
	s2.Restore(sp)
	got, _ = GetAccount(s2, "alice")
	if got.Balance != 100 {
		t.Fatalf("after Restore: balance=%d want 100", got.Balance)
	}
}

// TestAccountStoreRestoreCreatedRow checks that a row created after a
// snapshot is fully removed by Restore.
func TestAccountStoreRestoreCreatedRow(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	sp := s.Snapshot()
	PutAccount(s, &Account{Name: "newbie", Balance: 50})
	s.Restore(sp)
	if has, _ := HasAccount(s, "newbie"); has {
		t.Fatalf("created row survived Restore")
	}
}

// TestAccountStoreGetReturnsCopy verifies Get returns a value-copy —
// caller mutations don't leak back into the store.
func TestAccountStoreGetReturnsCopy(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	PutAccount(s, &Account{Name: "alice", Balance: 100})
	got, _ := GetAccount(s, "alice")
	got.Balance = 99999
	got2, _ := GetAccount(s, "alice")
	if got2.Balance != 100 {
		t.Fatalf("Get must return copy: got %d after caller mutation, want 100", got2.Balance)
	}
}

// TestAccountStoreRange checks RangeAccounts visits every account in
// alphabetical order.
func TestAccountStoreRange(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	for _, n := range []string{"bob", "alice", "carol"} {
		PutAccount(s, &Account{Name: auth.AccountName(n)})
	}
	var seen []string
	_ = RangeAccounts(s, func(a *Account) (bool, error) {
		seen = append(seen, string(a.Name))
		return true, nil
	})
	want := []string{"alice", "bob", "carol"}
	if !equalStrings(seen, want) {
		t.Fatalf("range: got %v want %v", seen, want)
	}
}

// TestCommentStoreRoundTrip is the typed-store basic put/get/delete.
func TestCommentStoreRoundTrip(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	c := Comment{Author: "alice", Permlink: "p1", NetRshares: 1234}
	PutComment(s, &c)

	got, err := GetComment(s, "alice", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if got.NetRshares != 1234 {
		t.Fatalf("rshares=%d want 1234", got.NetRshares)
	}

	DeleteComment(s, "alice", "p1")
	if has, _ := HasComment(s, "alice", "p1"); has {
		t.Fatalf("comment survived Delete")
	}
}

// TestCommentStoreByRoot checks the per-root index — root + replies
// are visible via RangeCommentsByRoot.
func TestCommentStoreByRoot(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	// Root post.
	PutComment(s, &Comment{Author: "alice", Permlink: "post1"})
	// Two replies under it.
	PutComment(s, &Comment{Author: "bob", Permlink: "reply1",
		ParentAuthor: "alice", ParentPermlink: "post1",
		RootAuthor: "alice", RootPermlink: "post1"})
	PutComment(s, &Comment{Author: "carol", Permlink: "reply2",
		ParentAuthor: "alice", ParentPermlink: "post1",
		RootAuthor: "alice", RootPermlink: "post1"})

	var seen []string
	_ = RangeCommentsByRoot(s, "alice", "post1", func(c *Comment) (bool, error) {
		seen = append(seen, string(c.Author))
		return true, nil
	})
	sort.Strings(seen)
	want := []string{"alice", "bob", "carol"}
	if !equalStrings(seen, want) {
		t.Fatalf("byRoot: got %v want %v", seen, want)
	}
}

// TestCommentStoreSnapshotRestore exercises the undo log on Put + Delete
// in sequence after a snapshot.
func TestCommentStoreSnapshotRestore(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	PutComment(s, &Comment{Author: "alice", Permlink: "p1", NetRshares: 100})
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}

	s2 := state.NewSession(db)
	sp := s2.Snapshot()
	// Modify + create a reply, then delete the original.
	PutComment(s2, &Comment{Author: "alice", Permlink: "p1", NetRshares: 999})
	PutComment(s2, &Comment{Author: "bob", Permlink: "reply",
		RootAuthor: "alice", RootPermlink: "p1"})
	DeleteComment(s2, "alice", "p1")
	s2.Restore(sp)

	got, err := GetComment(s2, "alice", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if got.NetRshares != 100 {
		t.Fatalf("after Restore: rshares=%d want 100", got.NetRshares)
	}
	if has, _ := HasComment(s2, "bob", "reply"); has {
		t.Fatalf("created reply survived Restore")
	}

	// byRoot index must also be restored.
	var seen []string
	_ = RangeCommentsByRoot(s2, "alice", "p1", func(c *Comment) (bool, error) {
		seen = append(seen, string(c.Author))
		return true, nil
	})
	if len(seen) != 1 || seen[0] != "alice" {
		t.Fatalf("byRoot after Restore: %v want [alice]", seen)
	}
}

// TestCommentVoteStoreRoundTrip is the basic put/get/delete.
func TestCommentVoteStoreRoundTrip(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	cv := CommentVote{Voter: "v1", Author: "a1", Permlink: "p", Rshares: 42}
	PutCommentVote(s, &cv)

	got, err := GetCommentVote(s, "v1", "a1", "p")
	if err != nil {
		t.Fatal(err)
	}
	if got.Rshares != 42 {
		t.Fatalf("rshares=%d want 42", got.Rshares)
	}

	DeleteCommentVote(s, "v1", "a1", "p")
	if has, _ := HasCommentVote(s, "v1", "a1", "p"); has {
		t.Fatalf("vote survived Delete")
	}
}

// TestCommentVoteStoreByComment checks the per-comment slice.
func TestCommentVoteStoreByComment(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	for _, voter := range []string{"v1", "v2", "v3"} {
		PutCommentVote(s, &CommentVote{
			Voter: auth.AccountName(voter), Author: "a", Permlink: "p", Rshares: 1,
		})
	}
	var seen []string
	_ = RangeCommentVotesByComment(s, "a", "p", func(cv *CommentVote) (bool, error) {
		seen = append(seen, string(cv.Voter))
		return true, nil
	})
	sort.Strings(seen)
	want := []string{"v1", "v2", "v3"}
	if !equalStrings(seen, want) {
		t.Fatalf("byComment: got %v want %v", seen, want)
	}
}

// TestCommentVoteStoreDeleteAllForComment checks the cashout-cleanup
// fast path drops every vote on (author, permlink) at once.
func TestCommentVoteStoreDeleteAllForComment(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	for _, voter := range []string{"v1", "v2", "v3"} {
		PutCommentVote(s, &CommentVote{
			Voter: auth.AccountName(voter), Author: "a", Permlink: "p",
		})
	}
	// And one vote on a DIFFERENT comment that should NOT be removed.
	PutCommentVote(s, &CommentVote{Voter: "v1", Author: "a", Permlink: "other"})

	DeleteAllCommentVotesForComment(s, "a", "p")
	for _, voter := range []string{"v1", "v2", "v3"} {
		if has, _ := HasCommentVote(s, auth.AccountName(voter), "a", "p"); has {
			t.Fatalf("vote %s survived DeleteAllForComment", voter)
		}
	}
	// The off-comment vote survives.
	if has, _ := HasCommentVote(s, "v1", "a", "other"); !has {
		t.Fatalf("DeleteAllForComment removed off-comment vote")
	}
}

// TestCommentVoteStoreSnapshotRestore exercises undo of mixed
// Put/Delete on the per-comment slice.
func TestCommentVoteStoreSnapshotRestore(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	PutCommentVote(s, &CommentVote{Voter: "v1", Author: "a", Permlink: "p", Rshares: 5})
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}

	s2 := state.NewSession(db)
	sp := s2.Snapshot()
	// Modify the existing vote, add a new one, delete a different one.
	PutCommentVote(s2, &CommentVote{Voter: "v1", Author: "a", Permlink: "p", Rshares: 99})
	PutCommentVote(s2, &CommentVote{Voter: "v2", Author: "a", Permlink: "p", Rshares: 7})
	s2.Restore(sp)

	got, err := GetCommentVote(s2, "v1", "a", "p")
	if err != nil {
		t.Fatal(err)
	}
	if got.Rshares != 5 {
		t.Fatalf("after Restore: rshares=%d want 5", got.Rshares)
	}
	if has, _ := HasCommentVote(s2, "v2", "a", "p"); has {
		t.Fatalf("created vote survived Restore")
	}
	// Per-comment slice restored.
	cnt := EnsureCommentVoteStore(s2).CountForComment("a", "p")
	if cnt != 1 {
		t.Fatalf("byComment count after Restore: %d want 1", cnt)
	}
}

// TestCommentVoteStoreDeleteAllRestore exercises undo of the bulk
// cleanup path.
func TestCommentVoteStoreDeleteAllRestore(t *testing.T) {
	db := openTempDB(t)
	s := state.NewSession(db)
	for _, voter := range []string{"v1", "v2", "v3"} {
		PutCommentVote(s, &CommentVote{
			Voter: auth.AccountName(voter), Author: "a", Permlink: "p", Rshares: 1,
		})
	}
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}

	s2 := state.NewSession(db)
	sp := s2.Snapshot()
	DeleteAllCommentVotesForComment(s2, "a", "p")
	if cnt := EnsureCommentVoteStore(s2).CountForComment("a", "p"); cnt != 0 {
		t.Fatalf("after DeleteAll: count=%d want 0", cnt)
	}
	s2.Restore(sp)

	for _, voter := range []string{"v1", "v2", "v3"} {
		if has, _ := HasCommentVote(s2, auth.AccountName(voter), "a", "p"); !has {
			t.Fatalf("vote %s missing after Restore", voter)
		}
	}
	if cnt := EnsureCommentVoteStore(s2).CountForComment("a", "p"); cnt != 3 {
		t.Fatalf("byComment count after Restore: %d want 3", cnt)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
