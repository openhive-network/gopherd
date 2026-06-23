package objects

import (
	"fmt"
	"math/rand"
	"testing"

	"hived-go/internal/protocol/auth"
)

const pcReserve = 512 << 20

func mkComment(author auth.AccountName, permlink string, root auth.AccountName, rootPL string, seed int) Comment {
	c := Comment{
		Author: author, Permlink: permlink,
		RootAuthor: root, RootPermlink: rootPL,
		NetRshares: int64(seed) * 97, CashoutTime: uint32(1600000000 + seed),
		ID: uint64(seed),
	}
	if root == author && rootPL == permlink {
		c.RootAuthor, c.RootPermlink = "", "" // self-root stored empty (matches chain)
	}
	return c
}
func commentBytes(c Comment) string            { return string(AppendComment(nil, &c)) }
func ckey(a auth.AccountName, p string) string { return string(a) + "|" + p }

// rootOf resolves a comment's root the way the by-root key does (self if empty).
func rootOf(c Comment) (auth.AccountName, string) {
	if c.RootAuthor == "" {
		return c.Author, c.Permlink
	}
	return c.RootAuthor, c.RootPermlink
}

// TestPersistCommentStore: equivalence vs reference over Put/Delete, the by-root
// ascending sibling scan, tx rollback, and restart without replay.
func TestPersistCommentStore(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenPersistCommentStore(dir, pcReserve)
	if err != nil {
		t.Fatal(err)
	}
	ref := map[string]Comment{}
	rng := rand.New(rand.NewSource(11))

	// A handful of root posts; replies hang under them.
	roots := []struct {
		a auth.AccountName
		p string
	}{{"alice", "root-a"}, {"bob", "root-b"}, {"carol", "root-c"}}
	// A reply's root is IMMUTABLE (fixed at creation), so derive it
	// deterministically from the reply's identity — re-Puts keep the same root.
	rootFor := func(author auth.AccountName, permlink string) int {
		h := 0
		for _, b := range []byte(string(author) + permlink) {
			h = h*31 + int(b)
		}
		if h < 0 {
			h = -h
		}
		return h % len(roots)
	}
	for i := 0; i < 5000; i++ {
		if rng.Intn(8) == 0 {
			r := roots[rng.Intn(len(roots))] // the root post itself
			c := mkComment(r.a, r.p, r.a, r.p, i)
			s.Put(&c)
			ref[ckey(r.a, r.p)] = c
			continue
		}
		author := auth.AccountName(fmt.Sprintf("u%03d", rng.Intn(150)))
		permlink := fmt.Sprintf("re-%04d", rng.Intn(2000))
		r := roots[rootFor(author, permlink)]
		if rng.Intn(10) == 0 {
			s.Delete(author, permlink)
			delete(ref, ckey(author, permlink))
		} else {
			c := mkComment(author, permlink, r.a, r.p, i)
			s.Put(&c)
			ref[ckey(author, permlink)] = c
		}
	}

	verify := func(st *PersistCommentStore, ctx string) {
		for k, want := range ref {
			got, ok := st.Get(want.Author, want.Permlink)
			if !ok || commentBytes(got) != commentBytes(want) {
				t.Fatalf("%s: get %q mismatch (ok=%v)", ctx, k, ok)
			}
		}
		// by-root scan: ascending, exact membership of each root's discussion.
		for _, r := range roots {
			var prevA, prevP string
			n := 0
			st.RangeByRoot(r.a, r.p, func(c *Comment) bool {
				if n > 0 && (string(c.Author) < prevA || (string(c.Author) == prevA && c.Permlink <= prevP)) {
					t.Fatalf("%s: root %s/%s not ascending at %s/%s", ctx, r.a, r.p, c.Author, c.Permlink)
				}
				prevA, prevP = string(c.Author), c.Permlink
				n++
				if commentBytes(*c) != commentBytes(ref[ckey(c.Author, c.Permlink)]) {
					t.Fatalf("%s: root-scan %s/%s mismatch", ctx, c.Author, c.Permlink)
				}
				return true
			})
			exp := 0
			for _, c := range ref {
				ra, rp := rootOf(c)
				if ra == r.a && rp == r.p {
					exp++
				}
			}
			if n != exp {
				t.Fatalf("%s: root %s/%s scan got %d want %d", ctx, r.a, r.p, n, exp)
			}
		}
	}
	verify(s, "fresh")

	// rollback
	s.SetUndoEnabled(true)
	sp := s.SnapshotPoint()
	for i := 0; i < 1000; i++ {
		author := auth.AccountName(fmt.Sprintf("u%03d", rng.Intn(150)))
		permlink := fmt.Sprintf("re-%04d", rng.Intn(2000))
		r := roots[rootFor(author, permlink)]
		c := mkComment(author, permlink, r.a, r.p, 80000+i)
		s.Put(&c)
	}
	s.RestoreToPoint(sp)
	s.SetUndoEnabled(false)
	verify(s, "after rollback")

	// restart
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenPersistCommentStore(dir, pcReserve)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	verify(s2, "after restart (no replay)")
	t.Logf("OK: %d live comments persisted + restored without replay", len(ref))
}
