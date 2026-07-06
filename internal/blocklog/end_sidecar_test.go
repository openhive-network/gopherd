package blocklog

import (
	"bytes"
	"fmt"
	"os"
	"testing"
)

// TestEndSidecarClampsCorruptTail: a log whose physical tail is garbage (an
// interrupted copy cut mid-block) must open cleanly when a "<main>.end"
// sidecar records the logical end — the index rebuild walks from the clamped
// size and the last block's endOffset fallback excludes the garbage.
func TestEndSidecarClampsCorruptTail(t *testing.T) {
	dir := t.TempDir()
	blocks := [][]byte{
		bytes.Repeat([]byte{0xa0}, 100),
		bytes.Repeat([]byte{0xa1}, 50),
		bytes.Repeat([]byte{0xa2}, 75),
	}
	mainPath := writeLegacyLog(t, dir, blocks)
	st, err := os.Stat(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	logicalEnd := st.Size()

	// Simulate the interrupted copy: garbage partial block at the tail, and
	// no index sidecar (the copy never had one).
	f, err := os.OpenFile(mainPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(bytes.Repeat([]byte{0xee}, 1234)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.Remove(mainPath + indexExtension); err != nil {
		t.Fatal(err)
	}

	// Without the sidecar, recoverValidEnd auto-detects the cut (highest
	// self-consistent trailer) — Open succeeds and reads all blocks.
	if r0, err := Open(mainPath); err != nil {
		t.Fatalf("Open with auto-recovery: %v", err)
	} else {
		if got := r0.Head(); got != 3 {
			t.Fatalf("auto-recovered Head = %d, want 3", got)
		}
		r0.Close()
		// Drop the persisted sidecar so the .end path below re-walks.
		if err := os.Remove(mainPath + indexExtension); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(mainPath+".end", []byte(fmt.Sprintf("%d\n", logicalEnd)), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Open(mainPath)
	if err != nil {
		t.Fatalf("Open with .end sidecar: %v", err)
	}
	defer r.Close()
	if got := r.Head(); got != 3 {
		t.Fatalf("Head = %d, want 3", got)
	}
	for i, want := range blocks {
		got, err := r.Block(uint32(i + 1))
		if err != nil {
			t.Fatalf("Block(%d): %v", i+1, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("block %d mismatch", i+1)
		}
	}

	// A bad sidecar (offset beyond the file) must fail loudly, not clamp.
	if err := os.WriteFile(mainPath+".end", []byte("999999999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(mainPath); err == nil {
		t.Fatal("expected Open to reject out-of-range .end sidecar")
	}
}
