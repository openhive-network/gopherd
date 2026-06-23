package snapshot

import (
	"testing"

	"hived-go/internal/state"
	"hived-go/internal/state/objects"
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
