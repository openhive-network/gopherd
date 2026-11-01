package objects

import (
	"bytes"
	"path/filepath"
	"reflect"
	"testing"

	"gopherd/internal/state"
)

// TestColdAccountingTrailerRoundTrip: a durable-cold comment/vote snapshot
// carries the cold bookkeeping, the load restores it exactly (bloom bits,
// coldTotal, coldRoot, coldCount), and a flipped byte in it fails the load
// instead of yielding a bloom with false negatives.
func TestColdAccountingTrailerRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cold")
	cold1, err := OpenPersistCold(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cold1.Close()
	_, csA := commentFixture(t, cold1)
	for _, e := range []struct{ a, p string }{{"replyer-a", "re-post"}, {"root2", "hello"}} {
		if !csA.Evict(e.a, e.p) {
			t.Fatalf("evict %s/%s", e.a, e.p)
		}
	}
	var buf bytes.Buffer
	if err := csA.MarshalSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	snap := buf.Bytes()

	load := func(b []byte) (*CommentStore, error) {
		s := state.NewSession(openTempDB(t))
		cs := EnsureCommentStore(s)
		cs.SetColdTier(cold1)
		return cs, cs.UnmarshalSnapshot(bytes.NewReader(b))
	}
	csB, err := load(snap)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if csB.coldTotal != csA.coldTotal {
		t.Fatalf("coldTotal %d want %d", csB.coldTotal, csA.coldTotal)
	}
	if !reflect.DeepEqual(csB.coldRoot, csA.coldRoot) {
		t.Fatalf("coldRoot %v want %v", csB.coldRoot, csA.coldRoot)
	}
	if csB.bloom == nil || csB.bloom.mask != csA.bloom.mask || csB.bloom.k != csA.bloom.k || !reflect.DeepEqual(csB.bloom.bits, csA.bloom.bits) {
		t.Fatal("bloom not restored bit for bit")
	}

	bad := append([]byte(nil), snap...)
	bad[len(bad)-1] ^= 0xff // last byte sits inside the trailer body
	if _, err := load(bad); err == nil {
		t.Fatal("corrupt trailer loaded without error")
	}

	_, cvsA := voteFixture(t, cold1)
	if n := cvsA.EvictComment("auth1", "post-a"); n == 0 {
		t.Fatal("no votes evicted")
	}
	buf.Reset()
	if err := cvsA.MarshalSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	cvsB := EnsureCommentVoteStore(state.NewSession(openTempDB(t)))
	cvsB.SetColdTier(cold1)
	if err := cvsB.UnmarshalSnapshot(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("vote load: %v", err)
	}
	if !reflect.DeepEqual(cvsB.coldCount, cvsA.coldCount) {
		t.Fatalf("coldCount %v want %v", cvsB.coldCount, cvsA.coldCount)
	}
}
