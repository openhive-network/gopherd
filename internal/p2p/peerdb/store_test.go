package peerdb

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorePersistAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.json")

	s, err := Open(path, Options{PersistEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	s.AddSticky("127.0.0.1:9876")
	s.Discover("127.0.0.1:9877")
	s.MarkAttempt("127.0.0.1:9877")
	s.MarkConnected("127.0.0.1:9877")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("peers.json not written: %v", err)
	}

	s2, err := Open(path, Options{PersistEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Len(); got != 2 {
		t.Errorf("after reload: got %d entries, want 2", got)
	}
	e, ok := s2.Get("127.0.0.1:9876")
	if !ok || !e.Sticky {
		t.Errorf("sticky entry not persisted: %+v ok=%v", e, ok)
	}
	e2, ok := s2.Get("127.0.0.1:9877")
	if !ok || e2.LastConnected.IsZero() || e2.ConnAttempts != 1 {
		t.Errorf("connected entry not persisted faithfully: %+v ok=%v", e2, ok)
	}
}

func TestStoreBackoffWithFakeClock(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: now}
	s := NewMemory(Options{
		PersistEvery: -1,
		Backoff: BackoffSchedule{Schedule: []time.Duration{
			1 * time.Second,
			5 * time.Second,
			30 * time.Second,
		}},
		Now: clock.Now,
	})
	const ep = "10.0.0.1:9876"
	s.Discover(ep)

	// Fresh entry → dialable immediately.
	got := s.DialableNow(nil)
	if len(got) != 1 || got[0] != ep {
		t.Fatalf("fresh entry not dialable: %v", got)
	}

	// One failure: 1s back-off active.
	s.MarkAttempt(ep)
	s.MarkFailed(ep)
	got = s.DialableNow(nil)
	if len(got) != 0 {
		t.Errorf("after 1 failure with no time advance, got dialable %v want []", got)
	}
	clock.advance(2 * time.Second)
	got = s.DialableNow(nil)
	if len(got) != 1 {
		t.Errorf("after 1 failure + 2s, got dialable %v want [ep]", got)
	}

	// Three failures: 30s back-off.
	s.MarkAttempt(ep)
	s.MarkFailed(ep)
	clock.advance(1 * time.Second)
	s.MarkAttempt(ep)
	s.MarkFailed(ep)
	got = s.DialableNow(nil)
	if len(got) != 0 {
		t.Errorf("after 3 failures + 1s, got dialable %v want []", got)
	}
	clock.advance(40 * time.Second)
	got = s.DialableNow(nil)
	if len(got) != 1 {
		t.Errorf("after 3 failures + 40s, got dialable %v want [ep]", got)
	}

	// MarkConnected resets failures.
	s.MarkConnected(ep)
	e, _ := s.Get(ep)
	if e.ConnFailures != 0 {
		t.Errorf("MarkConnected didn't zero failures: %d", e.ConnFailures)
	}
}

func TestStoreSkipSet(t *testing.T) {
	s := NewMemory(Options{PersistEvery: -1})
	s.AddSticky("a:1")
	s.Discover("b:2")
	s.Discover("c:3")
	skip := map[string]struct{}{"b:2": {}}
	got := s.DialableNow(skip)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %v", len(got), got)
	}
	for _, e := range got {
		if e == "b:2" {
			t.Errorf("skip endpoint b:2 leaked into dialable set")
		}
	}
}

func TestStoreStickyAlwaysDialable(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: now}
	s := NewMemory(Options{
		PersistEvery: -1,
		Backoff: BackoffSchedule{Schedule: []time.Duration{
			1 * time.Hour,
		}},
		Now: clock.Now,
	})
	s.AddSticky("seed:1")
	// Drive its failure count up.
	for i := 0; i < 10; i++ {
		s.MarkAttempt("seed:1")
		s.MarkFailed("seed:1")
	}
	got := s.DialableNow(nil)
	if len(got) != 1 || got[0] != "seed:1" {
		t.Errorf("sticky entry should always be dialable: %v", got)
	}
}

func TestStoreDiscoverIdempotent(t *testing.T) {
	s := NewMemory(Options{PersistEvery: -1})
	s.Discover("x:1")
	s.Discover("x:1")
	if s.Len() != 1 {
		t.Errorf("Discover same endpoint twice → len %d, want 1", s.Len())
	}
}

func TestStoreBackoffDelayCurve(t *testing.T) {
	b := DefaultBackoff
	cases := []struct {
		fails int
		want  time.Duration
	}{
		{0, 0},
		{1, 1 * time.Second},
		{2, 5 * time.Second},
		{3, 30 * time.Second},
		{4, 5 * time.Minute},
		{99, 5 * time.Minute}, // saturates at last entry
	}
	for _, c := range cases {
		if got := b.Delay(c.fails); got != c.want {
			t.Errorf("Delay(%d) = %v, want %v", c.fails, got, c.want)
		}
	}
}

type fakeClock struct {
	t time.Time
}

func (c *fakeClock) Now() time.Time      { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
