package objects

import (
	"fmt"

	"hived-go/internal/state/persiststore"
)

// PersistCold is a coldStore backed by persiststore — a DROP-IN replacement for
// mmapColdKV that SURVIVES RESTART. mmapColdKV opens cold.dat with O_TRUNC and
// keeps its index only in RAM, so the cold tier (the bulk of state — the
// comments/votes evicted from the resident set) is wiped on every start, which
// is the core reason a restart needs a full replay. PersistCold uses the same
// mmap design but append-only + index restored on Open (clean sidecar, or scan
// rebuild after a crash), so the cold tier comes back at head with no replay.
//
// It plugs into the existing seam: OpenColdTier(...) → SetColdTier(...). The
// comment/vote stores are unchanged; only the backend differs.
//
// Table routing matches mmapColdKV: the secondary-index tables
// (comment-by-root, vote-by-comment) are registered as ordered so rangePrefix
// works; the primary point-op tables stay map-only.
type PersistCold struct {
	ps *persiststore.Store
}

// persistColdReserve is the sparse mmap reservation (only touched pages cost
// disk). Matches mmapColdFileSize's sizing rationale for a full 60M replay.
const persistColdReserve = 400 << 30

// OpenPersistCold opens (and thereby restores) a restart-surviving cold tier.
func OpenPersistCold(dir string) (*PersistCold, error) {
	if dir == "" {
		return nil, fmt.Errorf("persistcold: dir required (a persistent cold tier needs a stable path)")
	}
	ps, err := persiststore.Open(dir, persistColdReserve)
	if err != nil {
		return nil, err
	}
	ps.RegisterOrderedTable(coldTableCommentByRoot)
	ps.RegisterOrderedTable(coldTableVoteByComment)
	return &PersistCold{ps: ps}, nil
}

// bset must not silently fail (Evict drops the row from RAM regardless); panic
// loudly on a full mapping, exactly like mmapColdKV.bset.
func (c *PersistCold) bset(key, val []byte) {
	if err := c.ps.Set(string(key), val); err != nil {
		panic(fmt.Sprintf("persistcold: %v — raise persistColdReserve or compact", err))
	}
}
func (c *PersistCold) bdel(key []byte)           { c.ps.Delete(string(key)) }
func (c *PersistCold) flushBatch()               {}
func (c *PersistCold) set(key, val []byte) error { return c.ps.Set(string(key), val) }
func (c *PersistCold) del(key []byte) error      { c.ps.Delete(string(key)); return nil }

func (c *PersistCold) get(key []byte) ([]byte, bool) { return c.ps.Get(string(key)) }
func (c *PersistCold) has(key []byte) bool           { return c.ps.Has(string(key)) }

func (c *PersistCold) deletePrefix(prefix []byte) error { c.ps.DeletePrefix(prefix); return nil }

func (c *PersistCold) rangePrefix(prefix []byte, fn func(key, val []byte) bool) error {
	c.ps.RangePrefix(prefix, func(k string, v []byte) bool { return fn([]byte(k), v) })
	return nil
}

func (c *PersistCold) rangePrimaryByOffset(prefix []byte, fn func(val []byte) bool) error {
	c.ps.RangePrimaryByOffset(prefix, fn)
	return nil
}

func (c *PersistCold) newPrefixIter(prefix []byte) (coldIterator, error) {
	return &persistColdIter{ps: c.ps, keys: c.ps.PrefixKeysSorted(prefix)}, nil
}

func (c *PersistCold) Dir() string    { return c.ps.Dir() }
func (c *PersistCold) Close() error   { return c.ps.Close() }
func (c *PersistCold) Flush() error   { return c.ps.Flush() }
func (c *PersistCold) Compact() error { return c.ps.Compact() }

// persistColdIter is a pull-style ascending iterator over a key prefix, backed
// by a sorted key snapshot (values fetched on demand from the live mapping).
type persistColdIter struct {
	ps   *persiststore.Store
	keys []string
	i    int
}

func (it *persistColdIter) valid() bool  { return it.i < len(it.keys) }
func (it *persistColdIter) key() []byte  { return []byte(it.keys[it.i]) }
func (it *persistColdIter) next()        { it.i++ }
func (it *persistColdIter) Close() error { return nil }
func (it *persistColdIter) val() ([]byte, error) {
	v, ok := it.ps.Get(it.keys[it.i])
	if !ok {
		return nil, fmt.Errorf("persistcold: iter value gone for key")
	}
	return v, nil
}
