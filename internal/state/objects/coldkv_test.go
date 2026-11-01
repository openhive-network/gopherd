package objects

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestColdKVBasic(t *testing.T) {
	c, err := openColdKV("")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()

	k := tableKey(coldTableCommentPrimary, []byte("alice/post1"))
	if _, ok := c.get(k); ok {
		t.Fatal("expected miss on empty store")
	}
	if c.has(k) {
		t.Fatal("has should be false on empty store")
	}
	if err := c.set(k, []byte("value1")); err != nil {
		t.Fatalf("set: %v", err)
	}
	v, ok := c.get(k)
	if !ok || !bytes.Equal(v, []byte("value1")) {
		t.Fatalf("get after set: ok=%v v=%q", ok, v)
	}
	if !c.has(k) {
		t.Fatal("has should be true after set")
	}
	if err := c.del(k); err != nil {
		t.Fatalf("del: %v", err)
	}
	if _, ok := c.get(k); ok {
		t.Fatal("expected miss after del")
	}
}

func TestColdKVRangePrefixOrderAndIsolation(t *testing.T) {
	c, err := openColdKV("")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()

	// Two tables sharing one store; ensure prefix scans don't cross tables
	// and iterate in ascending key order.
	rootA := []byte("rootA/p")
	c.set(tableKey(coldTableCommentByRoot, append(append([]byte{}, rootA...), []byte("\x00zeb/r")...)), []byte("z"))
	c.set(tableKey(coldTableCommentByRoot, append(append([]byte{}, rootA...), []byte("\x00abe/r")...)), []byte("a"))
	c.set(tableKey(coldTableCommentByRoot, append(append([]byte{}, rootA...), []byte("\x00mid/r")...)), []byte("m"))
	// A vote-table entry that must NOT show up in the by-root scan.
	c.set(tableKey(coldTableVotePrimary, append(append([]byte{}, rootA...), []byte("\x00abe/r")...)), []byte("vote"))

	prefix := tableKey(coldTableCommentByRoot, rootA)
	var got []string
	if err := c.rangePrefix(prefix, func(key, val []byte) bool {
		got = append(got, string(val))
		return true
	}); err != nil {
		t.Fatalf("rangePrefix: %v", err)
	}
	want := []string{"a", "m", "z"}
	if len(got) != len(want) {
		t.Fatalf("scan count: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scan order: got %v want %v", got, want)
		}
	}

	// deletePrefix removes only the matched table's range.
	if err := c.deletePrefix(prefix); err != nil {
		t.Fatalf("deletePrefix: %v", err)
	}
	var after int
	c.rangePrefix(prefix, func(key, val []byte) bool { after++; return true })
	if after != 0 {
		t.Fatalf("deletePrefix left %d entries", after)
	}
	// Vote-table entry survived (different table).
	if !c.has(tableKey(coldTableVotePrimary, append(append([]byte{}, rootA...), []byte("\x00abe/r")...))) {
		t.Fatal("deletePrefix wrongly removed a different table's entry")
	}
}

func TestPrefixUpperBound(t *testing.T) {
	cases := []struct{ in, want []byte }{
		{[]byte{0x01, 0x02}, []byte{0x01, 0x03}},
		{[]byte{0x01, 0xff}, []byte{0x02}},
		{[]byte{0xff, 0xff}, nil},
		{[]byte{0x05}, []byte{0x06}},
	}
	for _, c := range cases {
		got := prefixUpperBound(c.in)
		if !bytes.Equal(got, c.want) {
			t.Fatalf("prefixUpperBound(%v)=%v want %v", c.in, got, c.want)
		}
	}
}

// TestColdKVCloseKeepsUnflushedRows: the tier runs without a WAL, so rows that
// only reached a memtable used to vanish at Close. A reopened directory must
// hold every row written before a clean Close, batched or not.
func TestColdKVCloseKeepsUnflushedRows(t *testing.T) {
	dir := t.TempDir()
	c, err := openColdKV(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	kSet := tableKey(coldTableCommentPrimary, []byte("alice/direct"))
	kBatch := tableKey(coldTableCommentPrimary, []byte("bob/batched"))
	if err := c.set(kSet, []byte("v1")); err != nil {
		t.Fatalf("set: %v", err)
	}
	c.bset(kBatch, []byte("v2")) // left in the open batch on purpose
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	c2, err := openColdKV(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	for k, want := range map[string]string{string(kSet): "v1", string(kBatch): "v2"} {
		v, ok := c2.get([]byte(k))
		if !ok || string(v) != want {
			t.Fatalf("after reopen %q: ok=%v v=%q, want %q", k, ok, v, want)
		}
	}
}

// TestColdKVCheckpointRestore: a checkpoint taken from a live tier restores
// into another directory with every row, including rows still in the open
// batch and the memtable at checkpoint time, and the checkpoint stays
// loadable after the restored tier is written to.
func TestColdKVCheckpointRestore(t *testing.T) {
	base := t.TempDir()
	live := filepath.Join(base, "cold")
	c, err := openColdKV(live)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	k1 := tableKey(coldTableCommentPrimary, []byte("alice/one"))
	k2 := tableKey(coldTableVotePrimary, []byte("bob/two"))
	if err := c.set(k1, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	c.bset(k2, []byte("v2"))
	ck := filepath.Join(base, "snap", "cold")
	if err := os.MkdirAll(filepath.Dir(ck), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ck); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if !IsColdCheckpoint(ck) {
		t.Fatal("IsColdCheckpoint false on a fresh checkpoint")
	}
	// The live tier moves on; the checkpoint must not see it.
	if err := c.set(k1, []byte("changed")); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	for round := 0; round < 2; round++ {
		if err := RestoreColdCheckpoint(ck, live); err != nil {
			t.Fatalf("round %d restore: %v", round, err)
		}
		r, err := openColdKV(live)
		if err != nil {
			t.Fatalf("round %d open restored: %v", round, err)
		}
		for k, want := range map[string]string{string(k1): "v1", string(k2): "v2"} {
			v, ok := r.get([]byte(k))
			if !ok || string(v) != want {
				t.Fatalf("round %d %q: ok=%v v=%q want %q", round, k, ok, v, want)
			}
		}
		// Write through the restored tier and compact, which rewrites and
		// deletes SSTs; the next round restores from the checkpoint again.
		if err := r.set(k1, []byte("again")); err != nil {
			t.Fatal(err)
		}
		if err := r.db.Compact([]byte{0x00}, []byte{0xff}, true); err != nil {
			t.Fatal(err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
