package objects

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ColdIDFile names the file that stamps a pebble cold directory with the
// token of the snapshot it matches. A snapshot's checkpoint carries one; a
// live cold dir carries one only between a clean shutdown and the next open
// (openColdKV takes it and removes it before any write).
const ColdIDFile = "GOPHERD_COLD_ID"

// ReadColdID returns the cold ID stamped on dir, or "" when there is none.
func ReadColdID(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ColdIDFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// WriteColdID stamps dir with token, durably. Only for a directory whose
// tier is closed and holds exactly the rows the token's snapshot omitted.
func WriteColdID(dir, token string) error {
	if token == "" {
		return fmt.Errorf("cold id: empty token")
	}
	tmp := filepath.Join(dir, ColdIDFile+".tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, ColdIDFile)); err != nil {
		return err
	}
	return syncDir(dir)
}

// takeColdID reads and removes dir's cold ID, durably.
func takeColdID(dir string) (string, error) {
	tok := ReadColdID(dir)
	if tok == "" {
		return "", nil
	}
	if err := os.Remove(filepath.Join(dir, ColdIDFile)); err != nil {
		return "", fmt.Errorf("cold id: remove: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return "", fmt.Errorf("cold id: sync: %w", err)
	}
	return tok, nil
}

// Token returns the ID of the snapshot whose cold rows this tier holds
// exactly, or "" once anything has been written since it was set.
func (c *coldKV) Token() string {
	if c == nil || c.dirty {
		return ""
	}
	return c.token
}

// Checkpoint writes a consistent, openable copy of the pebble cold tier into
// dir, which must not exist yet, stamped with token. SSTs are hard-linked
// (copied when dir is on another filesystem); the MANIFEST and OPTIONS are
// copied. The memtables are flushed first: the tier runs without a WAL, so
// unflushed rows would not be in any file a checkpoint can capture.
//
// After it returns the live tier's Token is token until its next write.
//
// The cost is a few seconds and, while the live tier compacts past them, the
// disk held by the SSTs the checkpoint keeps alive. It replaces serializing
// every cold comment into the snapshot file (tens of GB) at each save.
func (c *coldKV) Checkpoint(dir, token string) error {
	if c == nil || c.db == nil {
		return fmt.Errorf("cold checkpoint: tier is closed")
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("cold checkpoint: flush: %w", err)
	}
	if err := c.db.Checkpoint(dir); err != nil {
		return fmt.Errorf("cold checkpoint: %w", err)
	}
	if err := WriteColdID(dir, token); err != nil {
		return fmt.Errorf("cold checkpoint: stamp: %w", err)
	}
	c.token, c.dirty = token, false
	return nil
}

// --- durableColdTier ---
//
// The pebble tier is durable for a snapshot exactly when it is stamped and
// untouched since: either the save just checkpointed it (the checkpoint
// carries the cold rows the snapshot leaves out), or it was opened from a
// stamped directory and the load has not written to it yet.

func (c *coldKV) durable() bool { return c.Token() != "" }

func (c *coldKV) countTable(table byte) int {
	n := 0
	c.rangeTableKeys(table, func([]byte) bool { n++; return true })
	return n
}

func (c *coldKV) rangeTableKeys(table byte, fn func(key []byte) bool) {
	_ = c.rangePrefix([]byte{table}, func(k, _ []byte) bool { return fn(k) })
}

// RestoreColdCheckpoint replaces the cold tier directory dst with the pebble
// checkpoint at src. dst is removed first; it must not be open.
//
// SSTs are immutable once written, so they are hard-linked (copied across
// filesystems) and the restore costs seconds. Every other file — MANIFEST,
// OPTIONS, CURRENT-style markers, WAL — is copied, because pebble appends to
// or rewrites those in place and the checkpoint must stay loadable after the
// restored tier has moved on.
func RestoreColdCheckpoint(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("cold restore: read checkpoint: %w", err)
	}
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("cold restore: clear %s: %w", dst, err)
	}
	// Build under a sibling temp dir and rename it into place, so an
	// interrupted restore never leaves a half-populated tier at dst.
	tmp := dst + ".restore"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return fmt.Errorf("cold restore: mkdir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		from, to := filepath.Join(src, e.Name()), filepath.Join(tmp, e.Name())
		if strings.HasSuffix(e.Name(), ".sst") {
			err = linkOrCopyFile(from, to)
		} else {
			err = copyFile(from, to)
		}
		if err != nil {
			_ = os.RemoveAll(tmp)
			return fmt.Errorf("cold restore: %s: %w", e.Name(), err)
		}
	}
	if err := syncDir(tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("cold restore: sync: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("cold restore: rename: %w", err)
	}
	return syncDir(filepath.Dir(dst))
}

// IsColdCheckpoint reports whether dir holds a pebble checkpoint (a pebble
// directory always has a MANIFEST).
func IsColdCheckpoint(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "MANIFEST-") {
			return true
		}
	}
	return false
}

func linkOrCopyFile(from, to string) error {
	if err := os.Link(from, to); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrExist) {
		// EXDEV (another filesystem), EPERM (no hard links), …: copy.
		return copyFile(from, to)
	} else {
		return err
	}
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// CheckpointCold captures the attached cold tier into dir for a snapshot
// save and returns the token that ties the snapshot to it. It returns ""
// with no error when there is nothing to capture this way — no cold tier, a
// backend other than pebble, or a save forced self-contained — in which case
// the snapshot streams its cold rows as before.
//
// The comment and vote stores share the one tier, so the comment store
// captures it for both.
func (s *CommentStore) CheckpointCold(dir string) (string, error) {
	c, ok := s.cold.(*coldKV)
	if !ok || SnapshotForceSelfContained {
		return "", nil
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw[:])
	if err := c.Checkpoint(dir, token); err != nil {
		_ = os.RemoveAll(dir)
		// A token left over from an earlier checkpoint or a stamped open
		// would still read as durable, and this save would then write the
		// resident set alone with no checkpoint behind it.
		c.dirty = true
		return "", err
	}
	return token, nil
}

// ColdToken returns the token of the snapshot whose cold rows the attached
// tier holds exactly, or "" (no tier, not pebble, or written to since).
func (s *CommentStore) ColdToken() string {
	if c, ok := s.cold.(*coldKV); ok {
		return c.Token()
	}
	return ""
}
