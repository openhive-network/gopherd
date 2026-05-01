package state

import (
	"bytes"

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
	tree *btree.BTreeG[memItem]
}

type memItem struct {
	key, value []byte
}

func openMem() *memBackend {
	return &memBackend{
		tree: btree.NewBTreeGOptions[memItem](
			func(a, b memItem) bool { return bytes.Compare(a.key, b.key) < 0 },
			btree.Options{Degree: 32, NoLocks: true},
		),
	}
}

func (b *memBackend) Close() error { return nil }

func (b *memBackend) rawGet(key []byte) ([]byte, error) {
	item, ok := b.tree.Get(memItem{key: key})
	if !ok {
		return nil, ErrNotFound
	}
	return item.value, nil
}

func (b *memBackend) rawHas(key []byte) (bool, error) {
	_, ok := b.tree.Get(memItem{key: key})
	return ok, nil
}

func (b *memBackend) rawIter(lower, upper []byte) (Iter, error) {
	mi := &memIter{tree: b.tree}
	mi.SetBounds(lower, upper)
	return mi, nil
}

func (b *memBackend) commit(overlay map[string]overlayEntry, _ bool) error {
	for k, e := range overlay {
		key := []byte(k)
		if e.tombstone {
			b.tree.Delete(memItem{key: key})
			continue
		}
		b.tree.Set(memItem{key: key, value: e.value})
	}
	return nil
}

func (b *memBackend) commitDirty(overlay map[string]overlayEntry, dirtyKeys []string, _ bool) error {
	// Walk the dirty-keys slice (cache-friendly, no Go map iter
	// overhead). Duplicate keys process the same final state from the
	// overlay map, so the second/third Set is a redundant no-op rather
	// than incorrect — small extra cost vs. a dedup-set allocation.
	for _, k := range dirtyKeys {
		e := overlay[k]
		key := []byte(k)
		if e.tombstone {
			b.tree.Delete(memItem{key: key})
			continue
		}
		b.tree.Set(memItem{key: key, value: e.value})
	}
	return nil
}

// memIter wraps tidwall/btree.IterG into our Iter interface. Reusable
// across SetBounds/First cycles within a Session.
type memIter struct {
	tree         *btree.BTreeG[memItem]
	it           btree.IterG[memItem]
	lower, upper []byte
	valid        bool
	hasIter      bool
}

func (m *memIter) SetBounds(lower, upper []byte) {
	if m.hasIter {
		m.it.Release()
		m.hasIter = false
	}
	// Copy bounds — the caller's lower/upper buffers may be reused.
	m.lower = append(m.lower[:0], lower...)
	m.upper = append(m.upper[:0], upper...)
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
	m.valid = bytes.Compare(m.it.Item().key, m.upper) < 0
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
	m.valid = bytes.Compare(m.it.Item().key, m.upper) < 0
	return m.valid
}

func (m *memIter) Key() []byte                  { return m.it.Item().key }
func (m *memIter) ValueAndErr() ([]byte, error) { return m.it.Item().value, nil }
func (m *memIter) Error() error                 { return nil }
func (m *memIter) Close() error {
	if m.hasIter {
		m.it.Release()
		m.hasIter = false
	}
	return nil
}
