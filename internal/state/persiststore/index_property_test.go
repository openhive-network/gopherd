package persiststore

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// TestIndexProperty hammers the store through the public API against a reference
// map across a long random op sequence — Set (insert + size-changing overwrite),
// Delete, interleaved with full-content verification, undo (Snapshot/Restore/
// Commit), Compact, and Flush+reopen. This is the safety net for the flat
// open-addressing index that replaced map[string]loc: any probe-chain,
// backward-shift, grow, or collision bug surfaces as a mismatch vs the map.
func TestIndexProperty(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	ref := map[string]string{}
	rng := rand.New(rand.NewSource(1))

	// Small-ish key universe so the table gets dense and probe chains get long
	// (exercises collisions + backward-shift hard). Keys vary in length.
	mkKey := func(n int) string { return fmt.Sprintf("k-%d-%0*d", n, n%37, n) }
	mkVal := func(n int) string { return fmt.Sprintf("v=%d:%0*d", n, n%90, n) }

	verify := func(tag string) {
		t.Helper()
		if s.Len() != len(ref) {
			t.Fatalf("%s: Len got %d want %d", tag, s.Len(), len(ref))
		}
		for k, v := range ref {
			got, ok := s.Get(k)
			if !ok || string(got) != v {
				t.Fatalf("%s: Get(%q) = %q,%v want %q", tag, k, got, ok, v)
			}
			if !s.Has(k) {
				t.Fatalf("%s: Has(%q) false", tag, k)
			}
		}
		// SortedKeys must equal the reference key set, sorted.
		want := make([]string, 0, len(ref))
		for k := range ref {
			want = append(want, k)
		}
		sort.Strings(want)
		got := s.SortedKeys()
		if len(got) != len(want) {
			t.Fatalf("%s: SortedKeys len %d want %d", tag, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: SortedKeys[%d]=%q want %q", tag, i, got[i], want[i])
			}
		}
	}

	for round := 0; round < 40; round++ {
		for op := 0; op < 1500; op++ {
			n := rng.Intn(1200) // key universe
			k := mkKey(n)
			if rng.Intn(3) == 0 {
				s.Delete(k)
				delete(ref, k)
			} else {
				v := mkVal(n*rng.Intn(7) + op) // size-changing overwrites
				if err := s.Set(k, []byte(v)); err != nil {
					t.Fatal(err)
				}
				ref[k] = v
			}
		}
		verify(fmt.Sprintf("round %d", round))

		// Exercise undo: snapshot, mutate, restore — must return to ref.
		sp := s.SnapshotPoint()
		s.SetUndoEnabled(true)
		undoRef := map[string]string{}
		for i := 0; i < 200; i++ {
			n := rng.Intn(1200)
			k := mkKey(n)
			if _, seen := undoRef[k]; !seen {
				if cur, ok := ref[k]; ok {
					undoRef[k] = cur
				} else {
					undoRef[k] = "\x00ABSENT"
				}
			}
			if rng.Intn(2) == 0 {
				s.Delete(k)
			} else {
				_ = s.Set(k, []byte(mkVal(i*99)))
			}
		}
		s.RestoreToPoint(sp)
		s.SetUndoEnabled(false)
		verify(fmt.Sprintf("round %d after undo", round))

		if round%7 == 3 {
			if err := s.Compact(); err != nil {
				t.Fatal(err)
			}
			verify(fmt.Sprintf("round %d after compact", round))
		}
		if round%5 == 2 {
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(dir, 1<<30)
			if err != nil {
				t.Fatal(err)
			}
			verify(fmt.Sprintf("round %d after flush+reopen", round))
		}
	}
	s.Close()
}
