package objects

import (
	"fmt"
	"math/rand"
	"testing"

	"gopherd/internal/protocol/auth"
)

const pvReserve = 512 << 20

func mkVote(voter, author auth.AccountName, permlink string, seed int) CommentVote {
	return CommentVote{
		Voter: voter, Author: author, Permlink: permlink,
		Weight: int16(seed%10000 + 1), Rshares: int64(seed) * 131,
		LastUpdate: uint32(1600000000 + seed), CurationWeight: uint64(seed) * 7,
	}
}
func voteBytes(cv CommentVote) string { return string(AppendCommentVote(nil, &cv)) }
func vkey(voter, author auth.AccountName, permlink string) string {
	return string(voter) + "|" + string(author) + "|" + permlink
}

// TestPersistCommentVoteStore: equivalence vs a reference over Put/Delete, the
// by-comment ascending-voter scan (distributeCuratorPool), tx rollback, and
// restart without replay.
func TestPersistCommentVoteStore(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenPersistCommentVoteStore(dir, pvReserve)
	if err != nil {
		t.Fatal(err)
	}
	ref := map[string]CommentVote{}
	rng := rand.New(rand.NewSource(7))

	authors := []auth.AccountName{"alice", "bob", "carol"}
	for i := 0; i < 6000; i++ {
		a := authors[rng.Intn(len(authors))]
		permlink := fmt.Sprintf("post-%02d", rng.Intn(15))
		voter := auth.AccountName(fmt.Sprintf("v%03d", rng.Intn(120)))
		k := vkey(voter, a, permlink)
		if rng.Intn(9) == 0 {
			s.Delete(voter, a, permlink)
			delete(ref, k)
		} else {
			cv := mkVote(voter, a, permlink, i)
			s.Put(&cv)
			ref[k] = cv
		}
	}

	verify := func(st *PersistCommentVoteStore, ctx string) {
		// point gets
		for k, want := range ref {
			got, ok := st.Get(want.Voter, want.Author, want.Permlink)
			if !ok || voteBytes(got) != voteBytes(want) {
				t.Fatalf("%s: get %q mismatch (ok=%v)", ctx, k, ok)
			}
		}
		// by-comment scan: ascending voter, exactly the comment's votes
		for _, a := range authors {
			for p := 0; p < 15; p++ {
				permlink := fmt.Sprintf("post-%02d", p)
				var prev string
				n := 0
				st.RangeByComment(a, permlink, func(cv *CommentVote) bool {
					if n > 0 && string(cv.Voter) <= prev {
						t.Fatalf("%s: %s/%s not ascending at %q", ctx, a, permlink, cv.Voter)
					}
					prev = string(cv.Voter)
					n++
					if voteBytes(*cv) != voteBytes(ref[vkey(cv.Voter, a, permlink)]) {
						t.Fatalf("%s: scan %s/%s voter %q mismatch", ctx, a, permlink, cv.Voter)
					}
					return true
				})
				exp := 0
				for _, cv := range ref {
					if cv.Author == a && cv.Permlink == permlink {
						exp++
					}
				}
				if n != exp {
					t.Fatalf("%s: %s/%s scan got %d want %d", ctx, a, permlink, n, exp)
				}
			}
		}
	}
	verify(s, "fresh")

	// tx rollback
	s.SetUndoEnabled(true)
	sp := s.SnapshotPoint()
	for i := 0; i < 1000; i++ {
		a := authors[rng.Intn(len(authors))]
		permlink := fmt.Sprintf("post-%02d", rng.Intn(15))
		voter := auth.AccountName(fmt.Sprintf("v%03d", rng.Intn(120)))
		cv := mkVote(voter, a, permlink, 50000+i)
		s.Put(&cv)
	}
	s.RestoreToPoint(sp)
	s.SetUndoEnabled(false)
	verify(s, "after rollback")

	// restart
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenPersistCommentVoteStore(dir, pvReserve)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	verify(s2, "after restart (no replay)")
	t.Logf("OK: %d live votes (×2 keys) persisted + restored without replay", len(ref))
}
