// Package corpus reads checked-in Go fuzz corpus files, so ordinary tests can
// assert things about the seeds that only hold for them (for example that a
// mainnet encoding round-trips exactly) without fuzzing.
package corpus

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Load returns the []byte inputs of every "go test fuzz v1" file under
// testdata/fuzz/<target> of the calling package, sorted by file name. It
// fails the test if a file is malformed and returns nil if the directory
// does not exist.
func Load(t testing.TB, target string) [][]byte {
	t.Helper()
	return LoadDir(t, filepath.Join("testdata", "fuzz", target))
}

// LoadDir is Load for an explicit corpus directory, for a fuzz target seeded
// from another package's corpus.
func LoadDir(t testing.TB, dir string) [][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([][]byte, 0, len(names))
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) != 2 || lines[0] != "go test fuzz v1" {
			t.Fatalf("%s: not a single-value go test fuzz v1 file", name)
		}
		lit, ok := strings.CutPrefix(lines[1], "[]byte(")
		if !ok || !strings.HasSuffix(lit, ")") {
			t.Fatalf("%s: value is not a []byte literal", name)
		}
		s, err := strconv.Unquote(strings.TrimSuffix(lit, ")"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out = append(out, []byte(s))
	}
	return out
}
