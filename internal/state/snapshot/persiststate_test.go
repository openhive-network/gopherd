package snapshot

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// TestSavePersistLoadRoundtrip is the zero-snapshot-node round-trip: the full
// non-cold state (K/V mem rows + every typed subsystem) is written into a
// persiststore container and restored into a fresh database — no snapshot file.
func TestSavePersistLoadRoundtrip(t *testing.T) {
	src := state.OpenMem()
	t.Cleanup(func() { _ = src.Close() })

	s := state.NewSession(src)
	s.Put(state.TableID(0x0001), state.PrimaryIndex, []byte("k1"), []byte("v1"))
	s.Put(state.TableID(0x0001), state.PrimaryIndex, []byte("k2"), []byte("v2"))
	ws := objects.EnsureWitnessStore(s)
	ws.Put(&objects.Witness{Owner: "alice", Created: 1234, TotalVotesVests: 1000, MaximumBlockSize: 65536})
	as := objects.EnsureAccountStore(s)
	as.Put(&objects.Account{Name: "alice", Balance: 100, HBDBalance: 50, VestingShares: 10000})
	as.Put(&objects.Account{Name: "bob", Balance: 200})
	cs := objects.EnsureCommentStore(s)
	cs.Put(&objects.Comment{Author: "alice", Permlink: "post1", Created: 7777})
	cvs := objects.EnsureCommentVoteStore(s)
	cvs.Put(&objects.CommentVote{Voter: "bob", Author: "alice", Permlink: "post1", Weight: 10000, Rshares: 12345})
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}

	dir := t.TempDir()
	if err := SavePersist(src, dir, 4242); err != nil {
		t.Fatalf("SavePersist: %v", err)
	}

	// Peek head without loading.
	if h, ok := PersistStateHead(dir); !ok || h != 4242 {
		t.Fatalf("PersistStateHead: got %d (ok=%v) want 4242", h, ok)
	}

	// Restore into a fresh db (subsystems pre-registered, like snapshot.Load).
	dst := state.OpenMem()
	t.Cleanup(func() { _ = dst.Close() })
	boot := state.NewSession(dst)
	objects.EnsureWitnessStore(boot)
	objects.EnsureAccountStore(boot)
	objects.EnsureCommentStore(boot)
	objects.EnsureCommentVoteStore(boot)

	head, err := LoadPersist(dst, dir)
	if err != nil {
		t.Fatalf("LoadPersist: %v", err)
	}
	if head != 4242 {
		t.Fatalf("LoadPersist head: got %d want 4242", head)
	}

	check := state.NewSession(dst)
	for k, want := range map[string]string{"k1": "v1", "k2": "v2"} {
		got, err := check.Get(state.TableID(0x0001), state.PrimaryIndex, []byte(k))
		if err != nil || string(got) != want {
			t.Fatalf("kv %s: got %q (err=%v) want %q", k, got, err, want)
		}
	}
	if a, ok := objects.EnsureAccountStore(check).Get("alice"); !ok || a.Balance != 100 || a.VestingShares != 10000 {
		t.Fatalf("account alice mismatch: ok=%v %+v", ok, a)
	}
	if w, ok := objects.EnsureWitnessStore(check).Get("alice"); !ok || w.TotalVotesVests != 1000 {
		t.Fatalf("witness alice mismatch: ok=%v %+v", ok, w)
	}
	if c, ok := objects.EnsureCommentStore(check).Get("alice", "post1"); !ok || c.Created != 7777 {
		t.Fatalf("comment mismatch: ok=%v created=%d", ok, c.Created)
	}
	if cv, ok := objects.EnsureCommentVoteStore(check).Get("bob", "alice", "post1"); !ok || cv.Rshares != 12345 {
		t.Fatalf("vote mismatch: ok=%v %+v", ok, cv)
	}
	t.Logf("zero-snapshot round-trip ok: head=%d, K/V + accounts + witnesses + comments + votes restored from persiststore container", head)
}

// TestSavePersistTwiceFreshContainer guards the atomic-swap fix: saving twice to
// the same dir must yield a container reflecting ONLY the second save (no stale
// rows from the first), proving the swap replaces rather than appends.
func TestSavePersistTwiceFreshContainer(t *testing.T) {
	src := state.OpenMem()
	t.Cleanup(func() { _ = src.Close() })
	s := state.NewSession(src)
	objects.EnsureAccountStore(s).Put(&objects.Account{Name: "alice", Balance: 100})
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir() + "/state"
	if err := SavePersist(src, dir, 100); err != nil {
		t.Fatalf("first save: %v", err)
	}

	// Mutate + save again at a new head.
	s2 := state.NewSession(src)
	objects.EnsureAccountStore(s2).Put(&objects.Account{Name: "alice", Balance: 999})
	objects.EnsureAccountStore(s2).Put(&objects.Account{Name: "carol", Balance: 7})
	if err := s2.Commit(false); err != nil {
		t.Fatal(err)
	}
	if err := SavePersist(src, dir, 200); err != nil {
		t.Fatalf("second save: %v", err)
	}
	if _, err := os.Stat(dir + ".tmp"); err == nil {
		t.Fatal("dir.tmp survived the swap")
	}

	dst := state.OpenMem()
	t.Cleanup(func() { _ = dst.Close() })
	objects.EnsureAccountStore(state.NewSession(dst))
	head, err := LoadPersist(dst, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if head != 200 {
		t.Fatalf("head: got %d want 200 (stale first save?)", head)
	}
	check := state.NewSession(dst)
	if a, ok := objects.EnsureAccountStore(check).Get("alice"); !ok || a.Balance != 999 {
		t.Fatalf("alice balance: got %d want 999 (second save not reflected)", a.Balance)
	}
	if _, ok := objects.EnsureAccountStore(check).Get("carol"); !ok {
		t.Fatal("carol missing (second save incomplete)")
	}
	t.Log("save-twice ok: fresh container reflects only the latest save, no stale rows, no leftover tmp")
}

// TestSavePersistChunksLargeValues covers the case the original round-trip test
// missed. SavePersist used to buffer each value WHOLE — all K/V rows in one
// bytes.Buffer, then every subsystem in another — which is invisible on a small
// fixture and fatal on a real chain: at mainnet scale that is multiple GB on top
// of the live heap, and it OOM-killed a node during shutdown, taking the
// container and the pending shutdown snapshot with it.
//
// This writes enough data to span several psChunkSize chunks and asserts the
// value round-trips exactly, so the chunk boundaries are actually exercised
// rather than assumed.
func TestSavePersistChunksLargeValues(t *testing.T) {
	src := state.OpenMem()
	t.Cleanup(func() { _ = src.Close() })

	s := state.NewSession(src)
	// ~3 chunks' worth of K/V rows, in pieces big enough to straddle boundaries
	// (value size is not a divisor of psChunkSize).
	const rowSize = 7919 // prime, so rows land across chunk edges
	rows := (psChunkSize * 3) / rowSize
	val := bytes.Repeat([]byte("x"), rowSize)
	for i := 0; i < rows; i++ {
		k := []byte(fmt.Sprintf("k%08d", i))
		s.Put(state.TableID(0x0001), state.PrimaryIndex, k, val)
	}
	objects.EnsureAccountStore(s).Put(&objects.Account{Name: "alice", Balance: 100})
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}

	dir := t.TempDir()
	if err := SavePersist(src, dir, 99); err != nil {
		t.Fatalf("SavePersist: %v", err)
	}

	dst := state.OpenMem()
	t.Cleanup(func() { _ = dst.Close() })
	ds := state.NewSession(dst)
	objects.EnsureAccountStore(ds)
	if err := ds.Commit(false); err != nil {
		t.Fatalf("pre-register commit: %v", err)
	}
	h, err := LoadPersist(dst, dir)
	if err != nil {
		t.Fatalf("LoadPersist: %v", err)
	}
	if h != 99 {
		t.Fatalf("head: got %d want 99", h)
	}

	// Every row must survive the chunk boundaries intact.
	check := state.NewSession(dst)
	for _, i := range []int{0, 1, rows / 2, rows - 2, rows - 1} {
		k := []byte(fmt.Sprintf("k%08d", i))
		got, err := check.Get(state.TableID(0x0001), state.PrimaryIndex, k)
		if err != nil {
			t.Fatalf("row %d missing after chunked round-trip: %v", i, err)
		}
		if !bytes.Equal(got, val) {
			t.Fatalf("row %d corrupted across a chunk boundary: got %d bytes want %d", i, len(got), len(val))
		}
	}
}
