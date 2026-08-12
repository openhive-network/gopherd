package objects

import (
	"os"
	"path/filepath"
	"testing"
)

// TestColdSidecarRoundTrip pins that a saved bloom comes back bit-identical.
//
// This matters more than a normal round-trip test: the bloom answers "might
// this comment be in the cold tier". A bit lost in serialisation becomes a
// FALSE NEGATIVE, and the caller reads that as "the comment does not exist" —
// consensus-visible, and silent.
func TestColdSidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := &CommentStore{bloom: newColdBloom(1 << 20)}

	keys := [][]byte{}
	for i := 0; i < 5000; i++ {
		k := append([]byte("author"), byte(i), byte(i>>8), byte(i>>16))
		s.bloom.add(k)
		s.coldTotal++
		keys = append(keys, k)
	}

	if err := s.SaveColdSidecar(dir, 42); err != nil {
		t.Fatalf("save: %v", err)
	}

	restored := &CommentStore{}
	ok, err := restored.LoadColdSidecar(dir, 42)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if restored.coldTotal != s.coldTotal {
		t.Fatalf("coldTotal: got %d want %d", restored.coldTotal, s.coldTotal)
	}
	// Every key that was added MUST still test positive. A bloom may report
	// false positives; a false negative here would be data loss.
	for i, k := range keys {
		if !restored.bloom.maybePresent(k) {
			t.Fatalf("key %d lost across save/load — this reads as 'comment missing'", i)
		}
	}
}

// TestColdSidecarRefusesWrongVintage pins that a sidecar from a different block
// is REJECTED rather than used.
//
// The cold tier and its bookkeeping must describe the same moment. Loading a
// bloom from a different vintage silently hides whatever rows were added since,
// so the load must fall back to the full scan instead of trusting it.
func TestColdSidecarRefusesWrongVintage(t *testing.T) {
	dir := t.TempDir()
	s := &CommentStore{bloom: newColdBloom(1 << 20)}
	s.bloom.add([]byte("x"))
	if err := s.SaveColdSidecar(dir, 100); err != nil {
		t.Fatalf("save: %v", err)
	}

	restored := &CommentStore{}
	ok, err := restored.LoadColdSidecar(dir, 101) // different block
	if ok {
		t.Fatal("must NOT accept a sidecar written for another block")
	}
	if err == nil {
		t.Fatal("a vintage mismatch should be reported, not silently ignored")
	}
}

// TestColdSidecarRefusesCorruption pins that a torn or corrupted file is
// rejected by the CRC rather than loaded as a valid-looking filter.
func TestColdSidecarRefusesCorruption(t *testing.T) {
	dir := t.TempDir()
	s := &CommentStore{bloom: newColdBloom(1 << 20)}
	for i := 0; i < 1000; i++ {
		s.bloom.add([]byte{byte(i), byte(i >> 8)})
	}
	if err := s.SaveColdSidecar(dir, 7); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Flip a byte in the middle of the bloom payload.
	p := ColdSidecarPath(dir)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	restored := &CommentStore{}
	ok, err := restored.LoadColdSidecar(dir, 7)
	if ok {
		t.Fatal("must NOT accept a corrupted sidecar")
	}
	if err == nil {
		t.Fatal("corruption should be reported so the caller knows it fell back")
	}
}

// TestColdSidecarAbsentIsNotAnError pins the first-run path: no sidecar simply
// means "do the full scan", which must not look like a failure.
func TestColdSidecarAbsentIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	s := &CommentStore{}
	ok, err := s.LoadColdSidecar(dir, 1)
	if ok {
		t.Fatal("reported success with no sidecar present")
	}
	if err != nil {
		t.Fatalf("absent sidecar must not be an error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, coldSidecarName)); !os.IsNotExist(statErr) {
		t.Fatal("test setup: expected no sidecar file")
	}
}
