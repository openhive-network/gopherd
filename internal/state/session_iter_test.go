package state

import (
	"runtime"
	"testing"
	"time"

	"gopherd/internal/pebblemem"
)

// rangeWithOverlay makes s open its pooled backend iterator: Range only uses
// it when the Session has overlay entries inside the ranged prefix.
func rangeWithOverlay(t *testing.T, s *Session) {
	t.Helper()
	s.Put(0x0100, 0, []byte("a"), []byte("1"))
	n := 0
	if err := s.Range(0x0100, 0, nil, func(body, value []byte) (bool, error) {
		n++
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("range saw %d rows, want 2 (one committed, one in the overlay)", n)
	}
}

// TestSessionCloseReleasesPooledIterator pins that Close releases the pooled
// pebble iterator of a Session that never commits, such as replay's invariant
// check or an API read.
func TestSessionCloseReleasesPooledIterator(t *testing.T) {
	db := openTempDB(t)
	seed := NewSession(db)
	seed.Put(0x0100, 0, []byte("b"), []byte("2"))
	if err := seed.Commit(false); err != nil {
		t.Fatal(err)
	}

	base := pebblemem.OpenIters.Load()
	s := NewSession(db)
	rangeWithOverlay(t, s)
	if got := pebblemem.OpenIters.Load() - base; got != 1 {
		t.Fatalf("%d pooled iterators open after Range, want 1", got)
	}
	s.Close()
	if got := pebblemem.OpenIters.Load() - base; got != 0 {
		t.Fatalf("%d pooled iterators open after Close, want 0", got)
	}
	// Still usable: the next Range opens a fresh iterator.
	rangeWithOverlay(t, s)
	s.Close()
}

// TestDroppedSessionReleasesPooledIterator pins that a Session dropped without
// Commit or Close still has its pooled iterator closed once it is collected.
// A pebble iterator pins every memtable live when it was made and has no
// finalizer: replay left such Sessions behind, the state DB's zombie memtables
// rose from 1 to 8 (128 MiB each) between blocks 29M and 36M, and a
// from-genesis replay was OOM-killed under a 20 GB cap near block 58M.
func TestDroppedSessionReleasesPooledIterator(t *testing.T) {
	db := openTempDB(t)
	seed := NewSession(db)
	seed.Put(0x0100, 0, []byte("b"), []byte("2"))
	if err := seed.Commit(false); err != nil {
		t.Fatal(err)
	}

	base := pebblemem.OpenIters.Load()
	const sessions = 20
	for i := 0; i < sessions; i++ {
		rangeWithOverlay(t, NewSession(db))
	}
	if got := pebblemem.OpenIters.Load() - base; got != sessions {
		t.Fatalf("%d pooled iterators open, want %d", got, sessions)
	}
	deadline := time.Now().Add(10 * time.Second)
	for pebblemem.OpenIters.Load() != base && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if got := pebblemem.OpenIters.Load() - base; got != 0 {
		t.Fatalf("%d pooled iterators still open after their Sessions were collected", got)
	}
}
