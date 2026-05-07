package state

import (
	"sort"

	"github.com/tidwall/btree"
)

// memBackend stores all key/value pairs in a single sorted in-memory
// btree. Mirrors chainbase's design: every read/write/iter is a
// pointer-deref-cost operation, no LSM merging, no compactions, no
// disk I/O. Trades persistence for ~3-5x replay throughput.
//
// Crash recovery: there is none. For the replay use case (the only
// caller today) we re-derive state from block_log on next start. To
// add persistence later, snapshot the tree to a flat file every N
// blocks and restore on startup.
//
// Thread safety: the backend is single-writer / single-reader, matching
// the rest of the replay pipeline. tidwall/btree is safe for concurrent
// reads but not for concurrent writes; if we ever multithread replay
// we'll need a RWMutex.
type memBackend struct {
	// tree keeps keys sorted for range scans (rawIter) AND point
	// lookups (rawGet/rawHas). A parallel hash map was tried but the
	// per-write mapassign cost in commitDirty (~9% of CPU at chain
	// head) exceeded the bsearch savings on reads, since most hot
	// reads (witnesses, accounts) are already absorbed by the
	// per-Database parsed-object caches.
	tree *btree.BTreeG[memItem]
	// commitHint is reused across commitDirty's SetHint/DeleteHint calls.
	// After sorting dirtyKeys ascending, adjacent operations land in the
	// same btree node ~all the time, so the hint short-circuits bsearch.
	commitHint btree.PathHint
	// commitBuf reuses the sort buffer across commits.
	commitBuf []string
}

type memItem struct {
	// key is stored as string so the btree's per-Set/Delete can avoid
	// the []byte allocation that string→[]byte conversion would force
	// on every commit. Strings are immutable, hashable, and Go's
	// runtime compares them via cmpstring (same SIMD path as cmpbody).
	key   string
	value []byte
}

func openMem() *memBackend {
	return &memBackend{
		tree: btree.NewBTreeGOptions[memItem](
			func(a, b memItem) bool { return a.key < b.key },
			btree.Options{Degree: 64, NoLocks: true},
		),
	}
}

func (b *memBackend) Close() error { return nil }

func (b *memBackend) rawGet(key []byte) ([]byte, error) {
	item, ok := b.tree.Get(memItem{key: string(key)})
	if !ok {
		return nil, ErrNotFound
	}
	return item.value, nil
}

func (b *memBackend) rawHas(key []byte) (bool, error) {
	_, ok := b.tree.Get(memItem{key: string(key)})
	return ok, nil
}

func (b *memBackend) rawIter(lower, upper []byte) (Iter, error) {
	mi := &memIter{tree: b.tree}
	mi.SetBounds(lower, upper)
	return mi, nil
}

func (b *memBackend) commit(overlay map[string]overlayEntry, _ bool) error {
	for k, e := range overlay {
		if e.tombstone {
			b.tree.Delete(memItem{key: k})
			continue
		}
		b.tree.Set(memItem{key: k, value: e.value})
	}
	return nil
}

func (b *memBackend) commitDirty(overlay map[string]overlayEntry, dirtyKeys []string, _ bool) error {
	// Sort dirtyKeys ascending so adjacent operations target neighboring
	// btree nodes — the reusable PathHint then short-circuits bsearch on
	// ~every call. Sorting also surfaces duplicates as adjacent so we
	// can skip the redundant Set/Delete (Session dedups consecutive
	// writes but not interleaved ones).
	keys := append(b.commitBuf[:0], dirtyKeys...)
	sort.Strings(keys)
	b.commitBuf = keys

	hint := &b.commitHint
	prev := ""
	first := true
	for _, k := range keys {
		if !first && k == prev {
			continue
		}
		first = false
		prev = k
		e := overlay[k]
		if e.tombstone {
			b.tree.DeleteHint(memItem{key: k}, hint)
			continue
		}
		b.tree.SetHint(memItem{key: k, value: e.value}, hint)
	}
	return nil
}

// memIter wraps tidwall/btree.IterG into our Iter interface. Reusable
// across SetBounds/First cycles within a Session.
type memIter struct {
	tree         *btree.BTreeG[memItem]
	it           btree.IterG[memItem]
	lower, upper string
	valid        bool
	hasIter      bool
}

func (m *memIter) SetBounds(lower, upper []byte) {
	if m.hasIter {
		m.it.Release()
		m.hasIter = false
	}
	// Stash bounds as strings — the btree's Less compares string keys
	// directly, so we avoid a per-Next bytes.Compare/cmpbody round-trip.
	m.lower = string(lower)
	m.upper = string(upper)
	m.valid = false
}

func (m *memIter) First() bool {
	if m.hasIter {
		m.it.Release()
	}
	m.it = m.tree.Iter()
	m.hasIter = true
	if !m.it.Seek(memItem{key: m.lower}) {
		m.valid = false
		return false
	}
	m.valid = m.it.Item().key < m.upper
	return m.valid
}

func (m *memIter) Valid() bool { return m.valid }

func (m *memIter) Next() bool {
	if !m.hasIter {
		return false
	}
	if !m.it.Next() {
		m.valid = false
		return false
	}
	m.valid = m.it.Item().key < m.upper
	return m.valid
}

func (m *memIter) Key() []byte                  { return []byte(m.it.Item().key) }
func (m *memIter) ValueAndErr() ([]byte, error) { return m.it.Item().value, nil }
func (m *memIter) Error() error                 { return nil }
func (m *memIter) Close() error {
	if m.hasIter {
		m.it.Release()
		m.hasIter = false
	}
	return nil
}
