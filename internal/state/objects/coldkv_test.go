package objects

import (
	"bytes"
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
