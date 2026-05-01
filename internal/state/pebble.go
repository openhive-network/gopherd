package state

import (
	"errors"

	"github.com/cockroachdb/pebble"
)

// pebbleBackend is the production backend backed by a Pebble LSM store.
type pebbleBackend struct {
	db *pebble.DB
}

func openPebble(path string, cacheSize int64) (*pebbleBackend, error) {
	opts := &pebble.Options{}
	if cacheSize > 0 {
		opts.Cache = pebble.NewCache(cacheSize)
		defer opts.Cache.Unref()
	}
	db, err := pebble.Open(path, opts)
	if err != nil {
		return nil, err
	}
	return &pebbleBackend{db: db}, nil
}

func (b *pebbleBackend) Close() error {
	if b.db == nil {
		return nil
	}
	err := b.db.Close()
	b.db = nil
	return err
}

func (b *pebbleBackend) rawGet(key []byte) ([]byte, error) {
	v, closer, err := b.db.Get(key)
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

func (b *pebbleBackend) rawHas(key []byte) (bool, error) {
	v, closer, err := b.db.Get(key)
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

func (b *pebbleBackend) rawIter(lower, upper []byte) (Iter, error) {
	it, err := b.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, err
	}
	return &pebbleIter{it: it}, nil
}

func (b *pebbleBackend) commitDirty(overlay map[string]overlayEntry, dirtyKeys []string, sync bool) error {
	if len(dirtyKeys) == 0 {
		return nil
	}
	batch := b.db.NewBatch()
	defer batch.Close()
	for _, k := range dirtyKeys {
		e := overlay[k]
		if e.tombstone {
			if err := batch.Delete([]byte(k), nil); err != nil {
				return err
			}
			continue
		}
		if err := batch.Set([]byte(k), e.value, nil); err != nil {
			return err
		}
	}
	opts := pebble.NoSync
	if sync {
		opts = pebble.Sync
	}
	return b.db.Apply(batch, opts)
}

func (b *pebbleBackend) commit(overlay map[string]overlayEntry, sync bool) error {
	if len(overlay) == 0 {
		return nil
	}
	batch := b.db.NewBatch()
	defer batch.Close()
	for k, e := range overlay {
		if e.tombstone {
			if err := batch.Delete([]byte(k), nil); err != nil {
				return err
			}
			continue
		}
		if err := batch.Set([]byte(k), e.value, nil); err != nil {
			return err
		}
	}
	opts := pebble.NoSync
	if sync {
		opts = pebble.Sync
	}
	return b.db.Apply(batch, opts)
}

// pebbleIter wraps *pebble.Iterator to satisfy the Iter interface. The
// wrapper is necessary only because pebble.Iterator.First returns a
// bool with the same semantics we want — we just expose Valid()
// separately for symmetry with the in-memory iterator.
type pebbleIter struct {
	it *pebble.Iterator
}

func (p *pebbleIter) SetBounds(lower, upper []byte)   { p.it.SetBounds(lower, upper) }
func (p *pebbleIter) First() bool                     { return p.it.First() }
func (p *pebbleIter) Valid() bool                     { return p.it.Valid() }
func (p *pebbleIter) Next() bool                      { return p.it.Next() }
func (p *pebbleIter) Key() []byte                     { return p.it.Key() }
func (p *pebbleIter) ValueAndErr() ([]byte, error)    { return p.it.ValueAndErr() }
func (p *pebbleIter) Error() error                    { return p.it.Error() }
func (p *pebbleIter) Close() error                    { return p.it.Close() }
