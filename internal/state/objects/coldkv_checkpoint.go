package objects

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Checkpoint writes a consistent, openable copy of the pebble cold tier into
// dir, which must not exist yet. SSTs are hard-linked (copied when dir is on
// another filesystem); the MANIFEST and OPTIONS are copied. The memtables are
// flushed first: the tier runs without a WAL, so unflushed rows would not be
// in any file a checkpoint can capture.
//
// The cost is a few seconds and, while the live tier compacts past them, the
// disk held by the SSTs the checkpoint keeps alive. It replaces serializing
// every cold comment into the snapshot file (tens of GB) at each save.
func (c *coldKV) Checkpoint(dir string) error {
	if c == nil || c.db == nil {
		return fmt.Errorf("cold checkpoint: tier is closed")
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("cold checkpoint: flush: %w", err)
	}
	if err := c.db.Checkpoint(dir); err != nil {
		return fmt.Errorf("cold checkpoint: %w", err)
	}
	return syncDir(dir)
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
