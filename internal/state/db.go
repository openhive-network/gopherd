package state

import (
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble"
)

// Database wraps a Pebble store with the table-aware key encoding used by
// the chainbase replacement.
type Database struct {
	db *pebble.DB
}

// Open opens or creates a Pebble store at path. cacheSize is the byte size
// of the block cache; pass 0 to use Pebble's default.
func Open(path string, cacheSize int64) (*Database, error) {
	opts := &pebble.Options{}
	if cacheSize > 0 {
		opts.Cache = pebble.NewCache(cacheSize)
		defer opts.Cache.Unref()
	}
	db, err := pebble.Open(path, opts)
	if err != nil {
		return nil, fmt.Errorf("state.Open %s: %w", path, err)
	}
	return &Database{db: db}, nil
}

// Close releases the database. After Close, all Sessions backed by this
// Database are invalid.
func (d *Database) Close() error {
	if d.db == nil {
		return nil
	}
	err := d.db.Close()
	d.db = nil
	return err
}

// rawGet fetches the raw value for a fully-encoded physical key, returning
// (nil, ErrNotFound) when the key is absent.
func (d *Database) rawGet(physicalKey []byte) ([]byte, error) {
	v, closer, err := d.db.Get(physicalKey)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer closer.Close()
	out := make([]byte, len(v))
	copy(out, v)
	return out, nil
}

// rawHas reports whether physicalKey exists, without copying the value.
func (d *Database) rawHas(physicalKey []byte) (bool, error) {
	v, closer, err := d.db.Get(physicalKey)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	closer.Close()
	_ = v
	return true, nil
}

// rawApply applies a Pebble write batch atomically.
func (d *Database) rawApply(b *pebble.Batch, sync bool) error {
	opts := pebble.NoSync
	if sync {
		opts = pebble.Sync
	}
	return d.db.Apply(b, opts)
}

// rawIter constructs a Pebble iterator over [lower, upper).
func (d *Database) rawIter(lower, upper []byte) (*pebble.Iterator, error) {
	return d.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
}

// ErrNotFound is returned when a key has no value in either the session
// overlay or the underlying Pebble store.
var ErrNotFound = errors.New("state: not found")
