package snapshot

import (
	"os"
	"path/filepath"
	"testing"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// TestSaveLoadRoundtrip writes a few rows to one in-memory database via
// both the K/V path and every typed-store, snapshots, then loads into a
// fresh database and confirms every value comes back identical.
func TestSaveLoadRoundtrip(t *testing.T) {
	src := state.OpenMem()
	t.Cleanup(func() { _ = src.Close() })

	// Populate K/V layer (DGPO-style singleton).
	s := state.NewSession(src)
	s.Put(state.TableID(0x0001), state.PrimaryIndex, []byte("k1"), []byte("v1"))
	s.Put(state.TableID(0x0001), state.PrimaryIndex, []byte("k2"), []byte("v2"))

	// Populate every typed store with at least one row so each path is
	// exercised. Use distinguishable names so a misload is visible.
	ws := objects.EnsureWitnessStore(s)
	ws.Put(&objects.Witness{Owner: "alice", Created: 1234, TotalVotesVests: 1000, MaximumBlockSize: 65536})
	ws.Put(&objects.Witness{Owner: "bob", Created: 1235, TotalVotesVests: 2000, MaximumBlockSize: 65536})

	as := objects.EnsureAccountStore(s)
	as.Put(&objects.Account{Name: "alice", Balance: 100, HBDBalance: 50, VestingShares: 10000})
	as.Put(&objects.Account{Name: "bob", Balance: 200})

	cs := objects.EnsureCommentStore(s)
	cs.Put(&objects.Comment{Author: "alice", Permlink: "post1", Created: 7777})
	cs.Put(&objects.Comment{Author: "alice", Permlink: "reply1", ParentAuthor: "alice", ParentPermlink: "post1",
		RootAuthor: "alice", RootPermlink: "post1"})

	cvs := objects.EnsureCommentVoteStore(s)
	cvs.Put(&objects.CommentVote{Voter: "bob", Author: "alice", Permlink: "post1", Weight: 10000, Rshares: 12345})

	wvs := objects.EnsureWitnessVoteStore(s)
	wvs.Put(auth.AccountName("alice"), auth.AccountName("bob"))
	wvs.Put(auth.AccountName("bob"), auth.AccountName("alice"))

	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Save snapshot.
	dir := t.TempDir()
	snapPath, err := Save(src, dir, 1000, "deadbeefcafe")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapPath, "manifest.json")); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapPath, "mem.bin")); err != nil {
		t.Fatalf("mem.bin missing: %v", err)
	}

	// Load into a fresh database. Pre-register every typed store so the
	// loader has a destination.
	dst := state.OpenMem()
	t.Cleanup(func() { _ = dst.Close() })
	bootstrap := state.NewSession(dst)
	objects.EnsureWitnessStore(bootstrap)
	objects.EnsureAccountStore(bootstrap)
	objects.EnsureCommentStore(bootstrap)
	objects.EnsureCommentVoteStore(bootstrap)
	objects.EnsureWitnessVoteStore(bootstrap)
	// Don't commit — registration is on the Database, not the Session.

	man, err := Load(dst, snapPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if man.BlockNum != 1000 {
		t.Errorf("block_num: got %d, want 1000", man.BlockNum)
	}
	if man.HeadBlockID != "deadbeefcafe" {
		t.Errorf("head_block_id: got %q, want deadbeefcafe", man.HeadBlockID)
	}

	// Verify every K/V row.
	check := state.NewSession(dst)
	for k, want := range map[string]string{"k1": "v1", "k2": "v2"} {
		got, err := check.Get(state.TableID(0x0001), state.PrimaryIndex, []byte(k))
		if err != nil {
			t.Fatalf("get %s: %v", k, err)
		}
		if string(got) != want {
			t.Errorf("kv %s: got %q, want %q", k, got, want)
		}
	}

	// Verify witness store.
	dws := objects.EnsureWitnessStore(check)
	for _, name := range []string{"alice", "bob"} {
		w, ok := dws.Get(auth.AccountName(name))
		if !ok {
			t.Errorf("witness %s missing after load", name)
			continue
		}
		// pull source for compare
		sw, _ := ws.Get(auth.AccountName(name))
		if w.Created != sw.Created || w.TotalVotesVests != sw.TotalVotesVests {
			t.Errorf("witness %s mismatch: %+v vs %+v", name, w, sw)
		}
	}

	// Verify account store.
	das := objects.EnsureAccountStore(check)
	for _, name := range []string{"alice", "bob"} {
		a, ok := das.Get(auth.AccountName(name))
		if !ok {
			t.Errorf("account %s missing after load", name)
			continue
		}
		sa, _ := as.Get(auth.AccountName(name))
		if a.Balance != sa.Balance || a.HBDBalance != sa.HBDBalance || a.VestingShares != sa.VestingShares {
			t.Errorf("account %s mismatch: balance=%d/%d hbd=%d/%d vests=%d/%d",
				name, a.Balance, sa.Balance, a.HBDBalance, sa.HBDBalance, a.VestingShares, sa.VestingShares)
		}
	}

	// Verify comment store.
	dcs := objects.EnsureCommentStore(check)
	if c, ok := dcs.Get(auth.AccountName("alice"), "post1"); !ok || c.Created != 7777 {
		t.Errorf("comment alice/post1 missing or wrong: ok=%v created=%d", ok, c.Created)
	}
	// byRoot rebuild check.
	rootCount := 0
	_ = dcs.RangeByRoot(auth.AccountName("alice"), "post1", func(c *objects.Comment) (bool, error) {
		rootCount++
		return true, nil
	})
	if rootCount != 2 {
		t.Errorf("byRoot rebuild: got %d comments, want 2", rootCount)
	}

	// Verify comment vote store.
	dcvs := objects.EnsureCommentVoteStore(check)
	if cv, ok := dcvs.Get(auth.AccountName("bob"), auth.AccountName("alice"), "post1"); !ok || cv.Weight != 10000 || cv.Rshares != 12345 {
		t.Errorf("vote bob→alice/post1 missing or wrong: ok=%v weight=%d rshares=%d", ok, cv.Weight, cv.Rshares)
	}
	// byComment rebuild check.
	voteCount := dcvs.CountForComment(auth.AccountName("alice"), "post1")
	if voteCount != 1 {
		t.Errorf("byComment rebuild: got %d votes, want 1", voteCount)
	}

	// Verify witness vote graph.
	dwvs := objects.EnsureWitnessVoteStore(check)
	if !dwvs.Has(auth.AccountName("alice"), auth.AccountName("bob")) {
		t.Errorf("witness vote alice→bob missing")
	}
	if !dwvs.Has(auth.AccountName("bob"), auth.AccountName("alice")) {
		t.Errorf("witness vote bob→alice missing")
	}
}

// TestLoadFailsWhenSubsystemUnregistered ensures the loader complains
// loudly if the snapshot has a subsystem the destination DB doesn't know
// about, instead of silently dropping data.
func TestLoadFailsWhenSubsystemUnregistered(t *testing.T) {
	src := state.OpenMem()
	t.Cleanup(func() { _ = src.Close() })
	s := state.NewSession(src)
	objects.EnsureWitnessStore(s).Put(&objects.Witness{Owner: "alice", Created: 1})
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	dir := t.TempDir()
	snapPath, err := Save(src, dir, 1, "")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	dst := state.OpenMem()
	t.Cleanup(func() { _ = dst.Close() })
	// Don't EnsureWitnessStore on dst — Load should fail.
	if _, err := Load(dst, snapPath); err == nil {
		t.Error("expected load to fail when subsystem missing")
	}
}

// TestDeterministicSnapshotBytes saves the same in-memory state twice
// and confirms the on-disk bytes (including manifest sha256s) are
// identical. This is the property the bisection workflow depends on:
// snapshot at block N from genesis must equal snapshot at block N from
// a snapshot+resume.
func TestDeterministicSnapshotBytes(t *testing.T) {
	build := func() *state.Database {
		d := state.OpenMem()
		s := state.NewSession(d)
		// Touch every typed store so the snapshot includes all of them.
		objects.EnsureWitnessStore(s).Put(&objects.Witness{Owner: "a", Created: 1, MaximumBlockSize: 65536})
		objects.EnsureWitnessStore(s).Put(&objects.Witness{Owner: "b", Created: 2, MaximumBlockSize: 65536})
		objects.EnsureAccountStore(s).Put(&objects.Account{Name: "a", Balance: 1})
		objects.EnsureAccountStore(s).Put(&objects.Account{Name: "b", Balance: 2})
		objects.EnsureCommentStore(s).Put(&objects.Comment{Author: "a", Permlink: "p", Created: 3})
		objects.EnsureCommentVoteStore(s).Put(&objects.CommentVote{Voter: "b", Author: "a", Permlink: "p", Weight: 4})
		objects.EnsureWitnessVoteStore(s).Put(auth.AccountName("a"), auth.AccountName("b"))
		s.Put(state.TableID(0x0001), state.PrimaryIndex, []byte("kv"), []byte("v"))
		if err := s.Commit(false); err != nil {
			t.Fatal(err)
		}
		return d
	}
	d1 := build()
	t.Cleanup(func() { _ = d1.Close() })
	d2 := build()
	t.Cleanup(func() { _ = d2.Close() })

	dir1 := t.TempDir()
	dir2 := t.TempDir()
	p1, err := Save(d1, dir1, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Save(d2, dir2, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mem.bin",
		"subsystems/witness_store.bin",
		"subsystems/account_store.bin",
		"subsystems/comment_store.bin",
		"subsystems/comment_vote_store.bin",
		"subsystems/witness_vote_store.bin",
	} {
		a, err := os.ReadFile(filepath.Join(p1, name))
		if err != nil {
			t.Fatalf("%s read: %v", name, err)
		}
		b, err := os.ReadFile(filepath.Join(p2, name))
		if err != nil {
			t.Fatalf("%s read: %v", name, err)
		}
		if string(a) != string(b) {
			t.Errorf("%s: snapshot bytes differ between identical builds", name)
		}
	}
}

// TestSaveCreatesAtomicallyNamedDir verifies the .tmp → final rename.
func TestSaveCreatesAtomicallyNamedDir(t *testing.T) {
	src := state.OpenMem()
	t.Cleanup(func() { _ = src.Close() })
	dir := t.TempDir()
	path, err := Save(src, dir, 42, "")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if filepath.Base(path) != "block-42" {
		t.Errorf("snapshot dir: got %q, want block-42", filepath.Base(path))
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp dir still present after save: %v", err)
	}
}

// TestSaveLoadRoundtripZstd re-runs the round-trip with GHIVE_SNAPSHOT_ZSTD
// semantics enabled: every file compressed on save, auto-detected on load,
// manifest checksums over the compressed bytes.
func TestSaveLoadRoundtripZstd(t *testing.T) {
	old := snapshotZstd
	snapshotZstd = true
	defer func() { snapshotZstd = old }()
	TestSaveLoadRoundtrip(t)
}
