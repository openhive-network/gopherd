package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestColdHeadStamp covers the guard on the persist-state fast-resume path.
//
// A persist-state resume is a PAIR: the container holds the non-cold typed
// state, the cold dir holds comments/votes, and before this stamp existed
// nothing tied them together — a mismatched pair loaded silently. That matters
// more here than in most systems because Hive blocks carry no state root, so a
// divergence never surfaces as an error; a producing node would simply sign
// blocks against wrong state.
//
// The rule is that an UNVERIFIABLE pair must be refused, not assumed good.
func TestColdHeadStamp(t *testing.T) {
	dir := t.TempDir()

	// Absent stamp (never cleanly closed, or written by an older build) must
	// report false — "cannot verify", which the caller treats as refuse.
	if _, ok := readColdHeadStamp(dir); ok {
		t.Fatal("readColdHeadStamp reported ok on a dir with no stamp; an unverifiable pair must be refused, not assumed good")
	}

	if err := writeColdHeadStamp(dir, 108892380); err != nil {
		t.Fatalf("writeColdHeadStamp: %v", err)
	}
	got, ok := readColdHeadStamp(dir)
	if !ok || got != 108892380 {
		t.Fatalf("round trip: got %d (ok=%v), want 108892380", got, ok)
	}

	// Overwrite must replace, not append/corrupt — shutdowns are repeated.
	if err := writeColdHeadStamp(dir, 108900000); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got, ok := readColdHeadStamp(dir); !ok || got != 108900000 {
		t.Fatalf("after rewrite: got %d (ok=%v), want 108900000", got, ok)
	}

	// No temp file left behind (the write is rename-atomic).
	if _, err := os.Stat(filepath.Join(dir, coldHeadStampName+".tmp")); err == nil {
		t.Fatal("stamp .tmp survived the rename")
	}

	// Garbage content must read as unverifiable rather than as some number.
	if err := os.WriteFile(filepath.Join(dir, coldHeadStampName), []byte("not-a-number"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := readColdHeadStamp(dir); ok {
		t.Fatal("garbage stamp reported ok; it must be treated as unverifiable")
	}

	// Empty coldDir (cold tier disabled) is not an error, just unverifiable.
	if err := writeColdHeadStamp("", 1); err != nil {
		t.Fatalf("writeColdHeadStamp(\"\"): %v", err)
	}
	if _, ok := readColdHeadStamp(""); ok {
		t.Fatal("empty coldDir reported a verifiable stamp")
	}
}
