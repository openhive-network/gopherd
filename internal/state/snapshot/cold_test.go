package snapshot

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

type coldFixture struct {
	db   *state.Database
	cold objects.ColdStore
}

func registerStores(db *state.Database) (*objects.CommentStore, *objects.CommentVoteStore) {
	s := state.NewSession(db)
	objects.EnsureWitnessStore(s)
	objects.EnsureAccountStore(s)
	objects.EnsureWitnessVoteStore(s)
	return objects.EnsureCommentStore(s), objects.EnsureCommentVoteStore(s)
}

func attachCold(t *testing.T, db *state.Database, dir string) objects.ColdStore {
	t.Helper()
	cold, err := objects.OpenColdTierBackend(dir, "pebble")
	if err != nil {
		t.Fatalf("open cold %s: %v", dir, err)
	}
	cs, cvs := registerStores(db)
	cs.SetColdTier(cold)
	cvs.SetColdTier(cold)
	return cold
}

// newColdSource builds a database whose comment and vote stores have most of
// their rows evicted to a pebble cold tier at dir.
func newColdSource(t *testing.T, dir string) coldFixture {
	t.Helper()
	db := state.OpenMem()
	t.Cleanup(func() { _ = db.Close() })
	cold := attachCold(t, db, dir)
	s := state.NewSession(db)
	cs := objects.EnsureCommentStore(s)
	cvs := objects.EnsureCommentVoteStore(s)
	for _, p := range []string{"cold-one", "cold-two", "hot"} {
		objects.PutComment(s, &objects.Comment{Author: "alice", Permlink: p, WasPaid: p != "hot", NetRshares: int64(len(p))})
		cvs.Put(&objects.CommentVote{Voter: "bob", Author: "alice", Permlink: p, Weight: 100, Rshares: 7})
	}
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"cold-one", "cold-two"} {
		if !cs.Evict("alice", p) {
			t.Fatalf("evict %s", p)
		}
		if cvs.EvictComment("alice", p) != 1 {
			t.Fatalf("evict votes of %s", p)
		}
	}
	return coldFixture{db: db, cold: cold}
}

func checkColdState(t *testing.T, db *state.Database) {
	t.Helper()
	s := state.NewSession(db)
	for _, p := range []string{"cold-one", "cold-two", "hot"} {
		c, err := objects.GetComment(s, "alice", p)
		if err != nil || c.NetRshares != int64(len(p)) {
			t.Fatalf("comment alice/%s after load: %+v, %v", p, c, err)
		}
		if _, err := objects.GetCommentVote(s, "bob", auth.AccountName("alice"), p); err != nil {
			t.Fatalf("vote on alice/%s after load: %v", p, err)
		}
	}
}

// TestSaveCheckpointsPebbleCold: a save over a pebble cold tier carries the
// tier as a checkpoint instead of streaming its rows, loads only over a cold
// dir prepared from that checkpoint, and then answers for every row.
func TestSaveCheckpointsPebbleCold(t *testing.T) {
	base := t.TempDir()
	src := newColdSource(t, filepath.Join(base, "cold"))
	path, err := Save(src.db, filepath.Join(base, "snaps"), 100, "")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := src.cold.Close(); err != nil {
		t.Fatal(err)
	}
	man, err := readManifest(filepath.Join(path, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if man.ColdToken == "" || man.ColdCheckpoint != ColdCheckpointDir {
		t.Fatalf("manifest has no cold checkpoint: %+v", man)
	}
	body, err := os.ReadFile(filepath.Join(path, "subsystems", "comment_store.bin"))
	if err != nil {
		t.Fatal(err)
	}
	// magic | version | residentField (bit 31 = durable cold) | coldHint
	if len(body) < 16 || string(body[0:4]) != "CMNT" {
		t.Fatalf("comment_store.bin header: % x", body[:min(len(body), 16)])
	}
	if rf := binary.LittleEndian.Uint32(body[8:12]); rf != 1<<31|1 {
		t.Fatalf("comment_store.bin resident field %#x, want durable flag + 1 resident record", rf)
	}

	// An unprepared cold dir must be refused, not loaded as an empty cold half.
	dst := state.OpenMem()
	t.Cleanup(func() { _ = dst.Close() })
	stray := attachCold(t, dst, filepath.Join(base, "stray"))
	if _, err := Load(dst, path); err == nil {
		t.Fatal("load over an unrelated cold tier succeeded")
	}
	_ = stray.Close()

	coldDir := filepath.Join(base, "cold2")
	how, err := PrepareColdDir(path, coldDir)
	if err != nil || how != ColdPrepRestored {
		t.Fatalf("prepare: %v, %v", how, err)
	}
	dst2 := state.OpenMem()
	t.Cleanup(func() { _ = dst2.Close() })
	cold2 := attachCold(t, dst2, coldDir)
	defer cold2.Close()
	if _, err := Load(dst2, path); err != nil {
		t.Fatalf("load over the restored checkpoint: %v", err)
	}
	checkColdState(t, dst2)

	// Read-only tools attach a scratch copy of the checkpoint.
	dst3 := state.OpenMem()
	t.Cleanup(func() { _ = dst3.Close() })
	registerStores(dst3)
	release, err := AttachSnapshotCold(dst3, path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := Load(dst3, path); err != nil {
		t.Fatalf("tool load: %v", err)
	}
	checkColdState(t, dst3)
}

// TestSaveForcedSelfContainedStreamsCold: SnapshotForceSelfContained still
// yields a snapshot that carries every row in its files and needs no cold dir.
func TestSaveForcedSelfContainedStreamsCold(t *testing.T) {
	objects.SnapshotForceSelfContained = true
	defer func() { objects.SnapshotForceSelfContained = false }()
	base := t.TempDir()
	src := newColdSource(t, filepath.Join(base, "cold"))
	defer src.cold.Close()
	path, err := Save(src.db, filepath.Join(base, "snaps"), 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok, _ := SnapshotColdToken(path); tok != "" {
		t.Fatal("forced self-contained save took a checkpoint")
	}
	if _, err := os.Stat(filepath.Join(path, ColdCheckpointDir)); !os.IsNotExist(err) {
		t.Fatalf("cold checkpoint dir present: %v", err)
	}
	dst := state.OpenMem()
	t.Cleanup(func() { _ = dst.Close() })
	registerStores(dst)
	if _, err := Load(dst, path); err != nil {
		t.Fatal(err)
	}
	checkColdState(t, dst)
}
