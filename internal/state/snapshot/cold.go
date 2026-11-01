package snapshot

import (
	"fmt"
	"os"
	"path/filepath"

	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// ColdPrep says what PrepareColdDir did.
type ColdPrep int

const (
	// ColdPrepNone: the snapshot streams its cold rows; the cold dir is left
	// to the caller.
	ColdPrepNone ColdPrep = iota
	// ColdPrepReused: the cold dir was already stamped with the snapshot's
	// token (a clean shutdown wrote both) and is used as it stands.
	ColdPrepReused
	// ColdPrepRestored: the cold dir was replaced by the snapshot's
	// checkpoint.
	ColdPrepRestored
)

func (p ColdPrep) String() string {
	switch p {
	case ColdPrepReused:
		return "reused in place"
	case ColdPrepRestored:
		return "restored from the snapshot's checkpoint"
	}
	return "not needed"
}

// SnapshotColdToken returns the cold token in snapDir's manifest, "" when the
// snapshot streams its cold rows.
func SnapshotColdToken(snapDir string) (string, error) {
	man, err := readManifest(filepath.Join(snapDir, "manifest.json"))
	if err != nil {
		return "", err
	}
	return man.ColdToken, nil
}

// PrepareColdDir makes coldDir hold the cold tier snapshotDir was saved with,
// before the caller opens it as a pebble cold tier and loads the snapshot.
//
// A coldDir stamped with the snapshot's token is left alone: that is the
// clean-shutdown case, and restart costs nothing. Otherwise coldDir is
// replaced by the snapshot's checkpoint, which hard-links the SSTs (copies
// them across filesystems). Either way the tier the load sees matches the
// snapshot; Load refuses any other.
func PrepareColdDir(snapDir, coldDir string) (ColdPrep, error) {
	man, err := readManifest(filepath.Join(snapDir, "manifest.json"))
	if err != nil {
		return ColdPrepNone, fmt.Errorf("snapshot: read manifest: %w", err)
	}
	if man.ColdToken == "" {
		return ColdPrepNone, nil
	}
	if coldDir == "" {
		return ColdPrepNone, fmt.Errorf("snapshot %s keeps its cold tier in a pebble checkpoint; loading it needs a --cold-dir to restore it into", snapDir)
	}
	if objects.ReadColdID(coldDir) == man.ColdToken {
		return ColdPrepReused, nil
	}
	ck := filepath.Join(snapDir, man.ColdCheckpoint)
	if !objects.IsColdCheckpoint(ck) {
		return ColdPrepNone, fmt.Errorf("snapshot %s: cold checkpoint %s is missing or not a pebble directory", snapDir, ck)
	}
	if objects.ReadColdID(ck) != man.ColdToken {
		return ColdPrepNone, fmt.Errorf("snapshot %s: cold checkpoint %s is stamped %q, manifest says %q", snapDir, ck, objects.ReadColdID(ck), man.ColdToken)
	}
	if err := objects.RestoreColdCheckpoint(ck, coldDir); err != nil {
		return ColdPrepNone, err
	}
	return ColdPrepRestored, nil
}

// AttachSnapshotCold is for read-only tools (state-jsonl, state-dump, …) that
// load a snapshot without a cold tier of their own. When the snapshot keeps
// its cold rows in a checkpoint, it restores the checkpoint into a scratch
// directory beside the snapshot (same filesystem, so hard links) and attaches
// it to the comment and vote stores registered on db. The returned cleanup
// closes and removes it; it is a no-op when nothing was attached.
func AttachSnapshotCold(db *state.Database, snapDir string) (func(), error) {
	tok, err := SnapshotColdToken(snapDir)
	if err != nil || tok == "" {
		return func() {}, err
	}
	scratch, err := os.MkdirTemp(filepath.Dir(filepath.Clean(snapDir)), ".cold-scratch-")
	if err != nil {
		return func() {}, err
	}
	coldDir := filepath.Join(scratch, "cold")
	if _, err := PrepareColdDir(snapDir, coldDir); err != nil {
		_ = os.RemoveAll(scratch)
		return func() {}, err
	}
	cold, err := objects.OpenColdTierBackend(coldDir, "pebble")
	if err != nil {
		_ = os.RemoveAll(scratch)
		return func() {}, err
	}
	s := state.NewSession(db)
	objects.EnsureCommentStore(s).SetColdTier(cold)
	objects.EnsureCommentVoteStore(s).SetColdTier(cold)
	return func() {
		_ = cold.Close()
		_ = os.RemoveAll(scratch)
	}, nil
}
