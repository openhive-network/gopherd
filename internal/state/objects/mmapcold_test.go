package objects

import (
	"bytes"
	"testing"
)

func TestMmapColdBasic(t *testing.T) {
	c, err := openMmapColdKV("")
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
	// Re-set (update) with a longer value — old blob becomes dead space.
	if err := c.set(k, []byte("value2-is-longer")); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if v, ok = c.get(k); !ok || !bytes.Equal(v, []byte("value2-is-longer")) {
		t.Fatalf("get after update: ok=%v v=%q", ok, v)
	}
	if err := c.del(k); err != nil {
		t.Fatalf("del: %v", err)
	}
	if _, ok := c.get(k); ok {
		t.Fatal("expected miss after del")
	}
}

func TestMmapColdRangePrefixIterIsolation(t *testing.T) {
	c, err := openMmapColdKV("")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()

	rootA := []byte("rootA/p")
	mk := func(suf string) []byte {
		return tableKey(coldTableCommentByRoot, append(append([]byte{}, rootA...), append([]byte{0x00}, suf...)...))
	}
	c.set(mk("zeb/r"), []byte("z"))
	c.set(mk("abe/r"), []byte("a"))
	c.set(mk("mid/r"), []byte("m"))
	// Vote-table entry that must NOT show up in the by-root scan.
	voteKey := tableKey(coldTableVotePrimary, append(append([]byte{}, rootA...), []byte("\x00abe/r")...))
	c.set(voteKey, []byte("vote"))

	prefix := tableKey(coldTableCommentByRoot, rootA)

	// rangePrefix: ascending order + table isolation.
	var got []string
	c.rangePrefix(prefix, func(_, val []byte) bool { got = append(got, string(val)); return true })
	if want := []string{"a", "m", "z"}; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("rangePrefix order/isolation: got %v", got)
	}

	// newPrefixIter: same order, positioned on first.
	it, err := c.newPrefixIter(prefix)
	if err != nil {
		t.Fatalf("newPrefixIter: %v", err)
	}
	var itgot []string
	for it.valid() {
		v, _ := it.val()
		itgot = append(itgot, string(v))
		it.next()
	}
	it.Close()
	if len(itgot) != 3 || itgot[0] != "a" || itgot[1] != "m" || itgot[2] != "z" {
		t.Fatalf("iterator order: got %v", itgot)
	}

	// deletePrefix: removes only the matched table.
	if err := c.deletePrefix(prefix); err != nil {
		t.Fatalf("deletePrefix: %v", err)
	}
	var after int
	c.rangePrefix(prefix, func(_, _ []byte) bool { after++; return true })
	if after != 0 {
		t.Fatalf("deletePrefix left %d entries", after)
	}
	if !c.has(voteKey) {
		t.Fatal("deletePrefix wrongly removed a different table's entry")
	}
}
