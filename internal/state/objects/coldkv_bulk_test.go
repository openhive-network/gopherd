package objects

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

func primKey(s string) []byte { return tableKey(coldTableCommentPrimary, []byte(s)) }

// TestColdBulkLoadOrdering: bulk-loaded rows end up exactly as the same
// writes through the batch path would leave them — across several SSTs per
// run, a run break on a key going backwards, an overwrite and a delete in
// mid-load, reads in mid-load, and a by-root row on the batch path.
func TestColdBulkLoadOrdering(t *testing.T) {
	defer func(v uint64) { bulkSSTTarget = v }(bulkSSTTarget)
	bulkSSTTarget = 4 << 10 // force several files per run

	dir := filepath.Join(t.TempDir(), "cold")
	c, err := openColdKV(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.beginBulkLoad(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	val := bytes.Repeat([]byte("x"), 200)
	// Run 1: sorted.
	for i := 0; i < 300; i++ {
		k := fmt.Sprintf("a%05d", i)
		c.bset(primKey(k), append([]byte(k), val...))
		want[k] = k + string(val)
	}
	// Run 2 starts below run 1's last key and overwrites one of its rows.
	c.bset(primKey("a00010"), []byte("overwritten"))
	want["a00010"] = "overwritten"
	for i := 0; i < 50; i++ {
		k := fmt.Sprintf("b%05d", i)
		c.bset(primKey(k), []byte(k))
		want[k] = k
	}
	// A delete in mid-load must not be resurrected by a later ingest.
	c.bdel(primKey("b00007"))
	delete(want, "b00007")
	// A read in mid-load sees what was written so far.
	if v, ok := c.get(primKey("b00049")); !ok || string(v) != "b00049" {
		t.Fatalf("mid-load read: ok=%v v=%q", ok, v)
	}
	// By-root rows go through the batch path.
	byRoot := tableKey(coldTableCommentByRoot, []byte("root\x00child"))
	c.bset(byRoot, []byte("br"))
	for i := 50; i < 60; i++ {
		k := fmt.Sprintf("b%05d", i)
		c.bset(primKey(k), []byte(k))
		want[k] = k
	}
	if err := c.endBulkLoad(); err != nil {
		t.Fatal(err)
	}
	if c.WriteErr() != nil {
		t.Fatal(c.WriteErr())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2, err := openColdKV(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	got := map[string]string{}
	if err := c2.rangePrefix([]byte{coldTableCommentPrimary}, func(k, v []byte) bool {
		got[string(k[1:])] = string(v)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("row %s = %q, want %q", k, got[k], w)
		}
	}
	if v, ok := c2.get(byRoot); !ok || string(v) != "br" {
		t.Fatalf("by-root row: ok=%v v=%q", ok, v)
	}
}

// TestSelfContainedLoadBulkIngest: a self-contained comment snapshot loaded
// into a pebble tier through the bulk path answers for every comment, with the
// same cold accounting as the store it was saved from.
func TestSelfContainedLoadBulkIngest(t *testing.T) {
	defer func(v uint64) { bulkSSTTarget = v }(bulkSSTTarget)
	bulkSSTTarget = 16 << 10

	srcCold, err := openColdKV("")
	if err != nil {
		t.Fatal(err)
	}
	defer srcCold.Close()
	db := openTempDB(t)
	s := state.NewSession(db)
	cs := EnsureCommentStore(s)
	cs.SetColdTier(srcCold)
	const n = 2000
	for i := 0; i < n; i++ {
		PutComment(s, &Comment{Author: auth.AccountName(fmt.Sprintf("u%04d", i%97)), Permlink: fmt.Sprintf("p%05d", i),
			WasPaid: i%5 != 0, NetRshares: int64(i)})
	}
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if i%5 != 0 && i%3 != 0 {
			cs.Evict(auth.AccountName(fmt.Sprintf("u%04d", i%97)), fmt.Sprintf("p%05d", i))
		}
	}
	SnapshotForceSelfContained = true
	var buf bytes.Buffer
	err = cs.MarshalSnapshot(&buf)
	SnapshotForceSelfContained = false
	if err != nil {
		t.Fatal(err)
	}

	dstCold, err := openColdKV(filepath.Join(t.TempDir(), "cold"))
	if err != nil {
		t.Fatal(err)
	}
	defer dstCold.Close()
	s2 := state.NewSession(openTempDB(t))
	cs2 := EnsureCommentStore(s2)
	cs2.SetColdTier(dstCold)
	cs2.SetResidentCap(10)
	if err := cs2.UnmarshalSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	if dstCold.bulk != nil {
		t.Fatal("bulk load left on after the snapshot load")
	}
	if cs2.Count() != n {
		t.Fatalf("count %d want %d", cs2.Count(), n)
	}
	for i := 0; i < n; i++ {
		c, err := GetComment(s2, auth.AccountName(fmt.Sprintf("u%04d", i%97)), fmt.Sprintf("p%05d", i))
		if err != nil || c.NetRshares != int64(i) {
			t.Fatalf("comment %d: %+v %v", i, c, err)
		}
	}
}
