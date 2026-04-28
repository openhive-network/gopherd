package state

import (
	"bytes"
	"testing"
)

func openTempDB(t *testing.T) *Database {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestKeyEncoding(t *testing.T) {
	k := EncodeKey(0x1234, 0x05, []byte{0xab, 0xcd})
	want := []byte{0x12, 0x34, 0x05, 0xab, 0xcd}
	if !bytes.Equal(k, want) {
		t.Fatalf("EncodeKey: got %x, want %x", k, want)
	}
	tbl, idx, body, ok := DecodeKey(k)
	if !ok || tbl != 0x1234 || idx != 0x05 || !bytes.Equal(body, []byte{0xab, 0xcd}) {
		t.Fatalf("DecodeKey: %v %v %x %v", tbl, idx, body, ok)
	}
}

func TestEncodeKeyIntoReusesBuffer(t *testing.T) {
	buf := make([]byte, 0, 32)
	buf = EncodeKeyInto(buf, 0x0001, 0, []byte("alice"))
	if string(buf[3:]) != "alice" {
		t.Fatalf("body mismatch: %s", buf[3:])
	}
	cap1 := cap(buf)
	buf = EncodeKeyInto(buf, 0x0001, 0, []byte("bob"))
	if cap(buf) != cap1 {
		t.Errorf("EncodeKeyInto reallocated: cap %d → %d", cap1, cap(buf))
	}
}

func TestSessionUncommittedReads(t *testing.T) {
	db := openTempDB(t)
	s := NewSession(db)
	s.Put(1, 0, []byte("k1"), []byte("v1"))
	got, err := s.Get(1, 0, []byte("k1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v1" {
		t.Fatalf("uncommitted read: got %q, want v1", got)
	}
}

func TestSessionUndoDropsWrites(t *testing.T) {
	db := openTempDB(t)
	s := NewSession(db)
	s.Put(1, 0, []byte("k1"), []byte("v1"))
	s.Undo()
	if _, err := s.Get(1, 0, []byte("k1")); err != ErrNotFound {
		t.Fatalf("after Undo, expected ErrNotFound, got err=%v", err)
	}
}

func TestSessionCommitPersists(t *testing.T) {
	db := openTempDB(t)
	{
		s := NewSession(db)
		s.Put(1, 0, []byte("k1"), []byte("v1"))
		s.Put(1, 0, []byte("k2"), []byte("v2"))
		if err := s.Commit(false); err != nil {
			t.Fatal(err)
		}
		if !s.Empty() {
			t.Fatal("session should be empty after Commit")
		}
	}
	// Fresh session — values must persist via Pebble.
	s2 := NewSession(db)
	got, err := s2.Get(1, 0, []byte("k1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v1" {
		t.Fatalf("got %q, want v1", got)
	}
}

func TestSessionDeleteThenGet(t *testing.T) {
	db := openTempDB(t)
	{
		s := NewSession(db)
		s.Put(1, 0, []byte("k1"), []byte("v1"))
		if err := s.Commit(false); err != nil {
			t.Fatal(err)
		}
	}
	s2 := NewSession(db)
	s2.Delete(1, 0, []byte("k1"))
	if _, err := s2.Get(1, 0, []byte("k1")); err != ErrNotFound {
		t.Fatalf("delete-in-overlay: expected ErrNotFound, got err=%v", err)
	}
	if err := s2.Commit(false); err != nil {
		t.Fatal(err)
	}
	s3 := NewSession(db)
	if _, err := s3.Get(1, 0, []byte("k1")); err != ErrNotFound {
		t.Fatalf("after Commit-with-Delete: expected ErrNotFound, got err=%v", err)
	}
}

func TestSessionTableIsolation(t *testing.T) {
	db := openTempDB(t)
	s := NewSession(db)
	s.Put(1, 0, []byte("k"), []byte("table-1"))
	s.Put(2, 0, []byte("k"), []byte("table-2"))
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}
	s2 := NewSession(db)
	v1, err := s2.Get(1, 0, []byte("k"))
	if err != nil || string(v1) != "table-1" {
		t.Fatalf("table 1 isolation: %s err=%v", v1, err)
	}
	v2, err := s2.Get(2, 0, []byte("k"))
	if err != nil || string(v2) != "table-2" {
		t.Fatalf("table 2 isolation: %s err=%v", v2, err)
	}
}

func TestSessionRangeMergesOverlayAndDB(t *testing.T) {
	db := openTempDB(t)
	{
		s := NewSession(db)
		s.Put(7, 0, []byte("alpha"), []byte("a"))
		s.Put(7, 0, []byte("bravo"), []byte("b"))
		s.Put(7, 0, []byte("delta"), []byte("d"))
		if err := s.Commit(false); err != nil {
			t.Fatal(err)
		}
	}
	s2 := NewSession(db)
	// Overlay overrides "bravo" with new value, deletes "delta", adds "charlie".
	s2.Put(7, 0, []byte("bravo"), []byte("BRAVO"))
	s2.Delete(7, 0, []byte("delta"))
	s2.Put(7, 0, []byte("charlie"), []byte("C"))

	got := map[string]string{}
	err := s2.Range(7, 0, nil, func(body, value []byte) (bool, error) {
		got[string(body)] = string(value)
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"alpha":   "a",
		"bravo":   "BRAVO",
		"charlie": "C",
		// "delta" deleted in overlay; must NOT appear.
	}
	if len(got) != len(want) {
		t.Fatalf("range count: got %d, want %d (got=%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("range[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestSessionRangeStop(t *testing.T) {
	db := openTempDB(t)
	s := NewSession(db)
	s.Put(1, 0, []byte("a"), []byte("1"))
	s.Put(1, 0, []byte("b"), []byte("2"))
	s.Put(1, 0, []byte("c"), []byte("3"))

	count := 0
	err := s.Range(1, 0, nil, func(body, value []byte) (bool, error) {
		count++
		return false, nil // stop after first
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("Range stop: visited %d entries, want 1", count)
	}
}
