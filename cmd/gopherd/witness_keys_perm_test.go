package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWitnessKeyFilePermissionWarning covers the check on the file holding
// BLOCK-SIGNING PRIVATE KEYS.
//
// Keys are deliberately not accepted as flags because command lines are visible
// in ps — but that only relocates the exposure to this file, which was never
// checked. A group- or world-readable key file lets any local user produce
// blocks as this witness.
func TestWitnessKeyFilePermissionWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.txt")
	// A syntactically valid line so loading proceeds past the permission check.
	const line = "alice 5JNHfZYKGaomSFvd4NUdQ9qMcEAC43kujbfjueTHpVapX1Kzq2n\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	captureStderr := func(fn func()) string {
		old := os.Stderr
		r, w, _ := os.Pipe()
		os.Stderr = w
		fn()
		w.Close()
		os.Stderr = old
		buf := make([]byte, 4096)
		n, _ := r.Read(buf)
		return string(buf[:n])
	}

	out := captureStderr(func() { _, _ = loadWitnessKeys(path) })
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "chmod 600") {
		t.Errorf("mode 0644 key file produced no permission warning; got %q", out)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	out = captureStderr(func() { _, _ = loadWitnessKeys(path) })
	if strings.Contains(out, "WARNING") {
		t.Errorf("mode 0600 key file must not warn; got %q", out)
	}
}
