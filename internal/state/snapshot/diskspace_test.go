package snapshot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotSizeEstimate(t *testing.T) {
	dir := t.TempDir()
	if got := snapshotSizeEstimate(dir); got != 0 {
		t.Fatalf("empty dir estimate = %d, want 0", got)
	}
	// Two sibling snapshots; the NEWEST (higher block) must be measured.
	old := filepath.Join(dir, "block-100", "subsystems")
	newer := filepath.Join(dir, "block-200", "subsystems")
	for d, size := range map[string]int{old: 1000, newer: 300} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "x.bin"), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "block-200", "mem.bin"), make([]byte, 50), 0o644); err != nil {
		t.Fatal(err)
	}
	// Ignore non-snapshot dirs and .tmp.
	if err := os.MkdirAll(filepath.Join(dir, "block-999.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := snapshotSizeEstimate(dir); got != 350 {
		t.Fatalf("estimate = %d, want 350 (newest sibling block-200)", got)
	}
}
