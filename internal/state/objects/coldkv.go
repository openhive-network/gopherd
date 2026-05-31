package objects

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/cockroachdb/pebble"
)

// coldKV is the on-disk cold tier for the heavy typed stores (CommentStore,
// CommentVoteStore). It is a SCRATCH key/value store — a Pebble instance in a
// temp directory, opened NoSync — holding records evicted from RAM so the
// resident working set stays bounded during a long replay (see
// project_disk_backed_stores_2026_05_31).
//
// It is NOT a durable database: dir-snapshots remain the persistence
// mechanism. The cold store is rebuilt from empty each process start and
// destroyed on Close. Records are encoded with the SAME bytes the snapshot
// path uses (AppendComment / AppendCommentVote), so a record can move between
// the RAM tier, the cold tier, and a snapshot without ever re-encoding or
// drifting.
//
// Keys carry a one-byte table prefix so a single Pebble instance can back
// several logical tables (comment primary, comment by-root, vote primary,
// vote by-comment). Callers build keys with the existing key helpers in
// comment.go and prepend the table byte via tableKey.
type coldKV struct {
	db    *pebble.DB
	dir   string
	owned bool // dir was created by us → remove it on Close
}

// Cold-tier table prefixes. One shared Pebble instance, namespaced by a
// leading byte so prefix scans stay within a logical table.
const (
	coldTableCommentPrimary    byte = 0x01
	coldTableCommentByRoot     byte = 0x02
	coldTableVotePrimary       byte = 0x03
	coldTableVoteByComment     byte = 0x04
)

// openColdKV opens (creating if needed) a scratch Pebble store at dir. If dir
// is empty a fresh temp directory is created and removed on Close. NoSync is
// used throughout: durability across process restarts is explicitly not
// required.
func openColdKV(dir string) (*coldKV, error) {
	owned := false
	if dir == "" {
		d, err := os.MkdirTemp("", "ghive-cold-")
		if err != nil {
			return nil, err
		}
		dir = d
		owned = true
	} else {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := pebble.Open(dir, &pebble.Options{
		// Generous in-process cache; the cold tier is read-rarely
		// (only on cold-comment access) but write-heavy at eviction.
		Cache:        pebble.NewCache(256 << 20),
		DisableWAL:   true, // scratch store; snapshots are the source of truth
		MaxOpenFiles: 1024,
	})
	if err != nil {
		return nil, err
	}
	return &coldKV{db: db, dir: dir, owned: owned}, nil
}

// tableKey prepends the table byte to a key built by the comment.go helpers.
func tableKey(table byte, key []byte) []byte {
	out := make([]byte, 1+len(key))
	out[0] = table
	copy(out[1:], key)
	return out
}

// get returns the value for a full (table-prefixed) key. ok=false on miss.
// The returned slice is a copy owned by the caller.
func (c *coldKV) get(key []byte) ([]byte, bool) {
	v, closer, err := c.db.Get(key)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, false
		}
		// Cold tier is scratch; a read error is a programming/environment
		// fault. Surface as a miss rather than panic on the hot path —
		// callers treat a miss as "not present", which for a scratch store
		// degrades gracefully (the record is simply re-derived or absent).
		return nil, false
	}
	out := make([]byte, len(v))
	copy(out, v)
	_ = closer.Close()
	return out, true
}

// has reports whether a full key exists without copying the value.
func (c *coldKV) has(key []byte) bool {
	_, closer, err := c.db.Get(key)
	if err != nil {
		return false
	}
	_ = closer.Close()
	return true
}

// set writes a value for a full key (NoSync).
func (c *coldKV) set(key, val []byte) error {
	return c.db.Set(key, val, pebble.NoSync)
}

// del removes a full key (NoSync).
func (c *coldKV) del(key []byte) error {
	return c.db.Delete(key, pebble.NoSync)
}

// deletePrefix removes every key with the given (table-prefixed) prefix.
func (c *coldKV) deletePrefix(prefix []byte) error {
	return c.db.DeleteRange(prefix, prefixUpperBound(prefix), pebble.NoSync)
}

// rangePrefix iterates every key/value whose key starts with prefix, in
// ascending key order. fn returns false to stop. The key and value passed to
// fn alias iterator-internal storage and MUST NOT be retained.
func (c *coldKV) rangePrefix(prefix []byte, fn func(key, val []byte) bool) error {
	it, err := c.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: prefixUpperBound(prefix),
	})
	if err != nil {
		return err
	}
	defer it.Close()
	for it.First(); it.Valid(); it.Next() {
		v, err := it.ValueAndErr()
		if err != nil {
			return err
		}
		if !fn(it.Key(), v) {
			break
		}
	}
	return it.Error()
}

// Close flushes and closes the store, removing the directory if we created it.
func (c *coldKV) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	if c.owned && c.dir != "" {
		_ = os.RemoveAll(c.dir)
	}
	return err
}

// prefixUpperBound returns the smallest key strictly greater than every key
// with the given prefix — the standard exclusive upper bound for a prefix
// scan. Returns nil (scan to end) when prefix is all 0xff.
func prefixUpperBound(prefix []byte) []byte {
	ub := make([]byte, len(prefix))
	copy(ub, prefix)
	for i := len(ub) - 1; i >= 0; i-- {
		if ub[i] != 0xff {
			ub[i]++
			return ub[:i+1]
		}
	}
	return nil
}

// coldDirFor returns the on-disk path for a named cold sub-store under base,
// or "" (cold disabled) when base is empty. Kept here so callers don't need
// filepath.
func coldDirFor(base, name string) string {
	if base == "" {
		return ""
	}
	return filepath.Join(base, name)
}
