package persiststore

import (
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
)

// msyncData flushes the data pages to disk (records durable) without writing the
// clean sidecar — part of simulating a crash.
func msyncData(s *Store) error { return unix.Msync(s.data, unix.MS_SYNC) }

// dropWithoutFlush unmaps + closes the file WITHOUT Flush — simulating a crash
// (dirty flag stays set on disk, no sidecar written).
func dropWithoutFlush(s *Store) {
	_ = unix.Munmap(s.data)
	s.data = nil
	_ = s.f.Close()
	s.f = nil
}

const testReserve = 256 << 20 // 256 MiB sparse

// TestRestartWithoutReplay is the core property: write records, Close (clean
// shutdown), reopen — every record is present and byte-identical, with NO
// replay. This is what the whole chainbase port exists to deliver.
func TestRestartWithoutReplay(t *testing.T) {
	dir := t.TempDir()
	want := map[string]string{}
	s, err := Open(dir, testReserve)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		k := fmt.Sprintf("account-%06d", i)
		v := fmt.Sprintf("balance=%d;vests=%d;meta=%0*d", i*7, i*13, i%40, i)
		if err := s.Set(k, []byte(v)); err != nil {
			t.Fatal(err)
		}
		want[k] = v
	}
	// Overwrite some (size-changing) to exercise realloc + free-list.
	for i := 0; i < 5000; i += 3 {
		k := fmt.Sprintf("account-%06d", i)
		v := fmt.Sprintf("UPDATED-balance=%d-with-a-longer-value-%0*d", i, i%100, i)
		if err := s.Set(k, []byte(v)); err != nil {
			t.Fatal(err)
		}
		want[k] = v
	}
	// Delete some.
	for i := 0; i < 5000; i += 7 {
		k := fmt.Sprintf("account-%06d", i)
		s.Delete(k)
		delete(want, k)
	}
	if s.Len() != len(want) {
		t.Fatalf("Len before close: got %d want %d", s.Len(), len(want))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// === RESTART: reopen, expect everything present without replay ===
	s2, err := Open(dir, testReserve)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	verifyAll(t, s2, want, "clean reopen (sidecar)")
}

// TestCrashRebuild: simulate a crash (dirty flag set, no clean sidecar) — the
// index must rebuild by scanning the data segment, still no replay.
func TestCrashRebuild(t *testing.T) {
	dir := t.TempDir()
	want := map[string]string{}
	s, err := Open(dir, testReserve)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		k := fmt.Sprintf("c-%05d", i)
		v := fmt.Sprintf("v-%d-%0*d", i, i%30, i)
		if err := s.Set(k, []byte(v)); err != nil {
			t.Fatal(err)
		}
		want[k] = v
	}
	// Overwrite (so a later record supersedes an earlier one for the same key).
	for i := 0; i < 3000; i += 2 {
		k := fmt.Sprintf("c-%05d", i)
		v := fmt.Sprintf("NEW-%d-%0*d", i, i%50, i)
		if err := s.Set(k, []byte(v)); err != nil {
			t.Fatal(err)
		}
		want[k] = v
	}
	// Simulate CRASH: msync the data + header-dirty, but DON'T write the
	// sidecar and DON'T clear dirty. (Set already marked dirty; just flush data
	// pages so the records are on disk, then drop the mapping without Flush.)
	if err := msyncData(s); err != nil {
		t.Fatal(err)
	}
	dropWithoutFlush(s)

	s2, err := Open(dir, testReserve)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.Len() != len(want) {
		t.Fatalf("crash rebuild Len: got %d want %d", s2.Len(), len(want))
	}
	verifyAll(t, s2, want, "crash rebuild (scan)")
}

func verifyAll(t *testing.T, s *Store, want map[string]string, ctx string) {
	t.Helper()
	if s.Len() != len(want) {
		t.Fatalf("%s: Len got %d want %d", ctx, s.Len(), len(want))
	}
	for k, v := range want {
		got, ok := s.Get(k)
		if !ok {
			t.Fatalf("%s: missing key %q", ctx, k)
		}
		if string(got) != v {
			t.Fatalf("%s: key %q got %q want %q", ctx, k, got, v)
		}
	}
}

// TestCompactReclaimsAndSurvives: heavy overwrite churn builds dead space;
// Compact reclaims it, preserves all live data, and the compacted store still
// restarts correctly.
func TestCompactReclaimsAndSurvives(t *testing.T) {
	dir := t.TempDir()
	want := map[string]string{}
	s, err := Open(dir, testReserve)
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 20; round++ {
		for i := 0; i < 1000; i++ {
			k := fmt.Sprintf("k-%04d", i)
			v := fmt.Sprintf("round%d-val-%0*d", round, i%50, i)
			if err := s.Set(k, []byte(v)); err != nil {
				t.Fatal(err)
			}
			want[k] = v
		}
	}
	deadBefore := s.DeadRatio()
	if deadBefore < 0.8 {
		t.Fatalf("expected heavy dead space after churn, got ratio %.2f", deadBefore)
	}
	if err := s.Compact(); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if dr := s.DeadRatio(); dr > 0.001 {
		t.Fatalf("dead ratio after compact: %.4f (want ~0)", dr)
	}
	verifyAll(t, s, want, "after compact (live)")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir, testReserve)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	verifyAll(t, s2, want, "reopen after compact")
	t.Logf("dead ratio before compact: %.2f → after: %.4f; %d live keys", deadBefore, s2.DeadRatio(), s2.Len())
}
