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

// TestUndoRollback: the SnapshotPoint/RestoreToPoint/CommitPoint contract —
// mutations after a savepoint are fully reverted (values, presence, and the
// liveBytes accounting), like a rolled-back transaction.
func TestUndoRollback(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, testReserve)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetUndoEnabled(true)

	// Baseline committed state.
	base := map[string]string{}
	for i := 0; i < 500; i++ {
		k := fmt.Sprintf("k-%03d", i)
		v := fmt.Sprintf("base-%d", i)
		s.Set(k, []byte(v))
		base[k] = v
	}
	s.CommitPoint()
	liveAtBase := s.liveBytes

	// === tx: savepoint, then a mix of overwrites, new keys, and deletes ===
	sp := s.SnapshotPoint()
	for i := 0; i < 500; i += 2 {
		s.Set(fmt.Sprintf("k-%03d", i), []byte(fmt.Sprintf("DIRTY-%d-xxxxxxxx", i)))
	}
	for i := 1000; i < 1100; i++ {
		s.Set(fmt.Sprintf("k-%04d", i), []byte("new"))
	}
	for i := 1; i < 100; i += 2 {
		s.Delete(fmt.Sprintf("k-%03d", i))
	}
	// Roll back the whole tx.
	s.RestoreToPoint(sp)

	// Must be byte-identical to base, same key count, same liveBytes.
	verifyAll(t, s, base, "after rollback")
	if s.liveBytes != liveAtBase {
		t.Fatalf("liveBytes after rollback: got %d want %d", s.liveBytes, liveAtBase)
	}

	// And the reverted state must still survive a restart.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir, testReserve)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	verifyAll(t, s2, base, "rollback + restart")
}

// TestOrderedPrefixScan: the secondary-index prefix scan (votes-by-comment
// style) — ascending order, table-routed (primary keys excluded), surviving
// restart (reseed after Open) and reflecting undo.
func TestOrderedPrefixScan(t *testing.T) {
	const tPrimary, tByComment byte = 0x01, 0x04
	dir := t.TempDir()
	s, err := Open(dir, testReserve)
	if err != nil {
		t.Fatal(err)
	}
	s.RegisterOrderedTable(tByComment)

	mk := func(table byte, k string) string { return string(table) + k }
	// Primary (map-only) keys — must NOT appear in prefix scans.
	for i := 0; i < 200; i++ {
		s.Set(mk(tPrimary, fmt.Sprintf("p%04d", i)), []byte("primary"))
	}
	// by-comment secondary keys: "comment<NN>|voter<MM>" → val.
	want := map[string][]string{} // comment -> sorted voter-vals
	for c := 0; c < 30; c++ {
		for v := 0; v < 20; v++ {
			key := mk(tByComment, fmt.Sprintf("comment%02d|voter%02d", c, v))
			val := fmt.Sprintf("c%02d-v%02d", c, v)
			s.Set(key, []byte(val))
			cp := fmt.Sprintf("comment%02d", c)
			want[cp] = append(want[cp], val)
		}
	}
	scanComment := func(st *Store, c int) []string {
		var got []string
		st.RangePrefix([]byte(mk(tByComment, fmt.Sprintf("comment%02d|", c))), func(_ string, val []byte) bool {
			got = append(got, string(val))
			return true
		})
		return got
	}
	checkAll := func(st *Store, ctx string) {
		for c := 0; c < 30; c++ {
			got := scanComment(st, c)
			exp := want[fmt.Sprintf("comment%02d", c)]
			if len(got) != len(exp) {
				t.Fatalf("%s: comment%02d got %d want %d", ctx, c, len(got), len(exp))
			}
			for i := range got {
				if got[i] != exp[i] { // exp built in ascending voter order = scan order
					t.Fatalf("%s: comment%02d[%d] got %q want %q", ctx, c, i, got[i], exp[i])
				}
			}
		}
	}
	checkAll(s, "fresh")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: reopen + re-register (reseeds the ordered index from the loaded
	// idx) → prefix scans must still work, no replay.
	s2, err := Open(dir, testReserve)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.RegisterOrderedTable(tByComment)
	checkAll(s2, "after restart")

	// Undo: delete some, add some, roll back → scans revert.
	s2.SetUndoEnabled(true)
	sp := s2.SnapshotPoint()
	s2.Delete(mk(tByComment, "comment05|voter10"))
	s2.Set(mk(tByComment, "comment05|voterZZ"), []byte("extra"))
	if n := len(scanComment(s2, 5)); n != 20 {
		t.Fatalf("during tx: comment05 got %d want 20 (−1 del +1 add)", n)
	}
	s2.RestoreToPoint(sp)
	checkAll(s2, "after rollback")
}
