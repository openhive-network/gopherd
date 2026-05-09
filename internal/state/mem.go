package state

import (
	"sort"

	"github.com/tidwall/btree"
)

// memBackend stores all key/value pairs in a per-table btree map.
// Per-table trees keep each table's working set contiguous in memory
// for better CPU cache utilization, and shrink the effective btree
// depth — at 3.5M blocks the chain holds ~50M CommentVote rows, ~5M
// Comments, ~400K Accounts. Stuffing them all in ONE btree pushed
// the depth into the cache-thrashing zone (log₂(50M)≈26 levels).
//
// Crash recovery: there is none. For the replay use case we re-derive
// state from block_log on next start.
type memBackend struct {
	// trees keys each table's data by the 3-byte [TableID:2 BE][IndexID:1]
	// header decoded from the physical key. Lazily-initialized.
	trees map[uint32]*btree.BTreeG[memItem]
	// commitHints reuses one PathHint per (table, index) across
	// commitDirty's SetHint/DeleteHint calls.
	commitHints map[uint32]*btree.PathHint
	// commitBuf reuses the sort buffer across commits.
	commitBuf []string
}

type memItem struct {
	key   string
	value []byte
}

// memTreeKey extracts the (TableID, IndexID) header from the first 3
// bytes of a physical key.
func memTreeKey(key []byte) uint32 {
	if len(key) < 3 {
		return 0
	}
	return uint32(key[0])<<16 | uint32(key[1])<<8 | uint32(key[2])
}

func memTreeKeyFromString(key string) uint32 {
	if len(key) < 3 {
		return 0
	}
	return uint32(key[0])<<16 | uint32(key[1])<<8 | uint32(key[2])
}

func openMem() *memBackend {
	return &memBackend{
		trees:       make(map[uint32]*btree.BTreeG[memItem], 32),
		commitHints: make(map[uint32]*btree.PathHint, 32),
	}
}

func (b *memBackend) getTree(bucket uint32) *btree.BTreeG[memItem] {
	if t, ok := b.trees[bucket]; ok {
		return t
	}
	t := btree.NewBTreeGOptions[memItem](
		func(a, b memItem) bool { return a.key < b.key },
		btree.Options{Degree: 64, NoLocks: true},
	)
	b.trees[bucket] = t
	return t
}

func (b *memBackend) getHint(bucket uint32) *btree.PathHint {
	if h, ok := b.commitHints[bucket]; ok {
		return h
	}
	h := &btree.PathHint{}
	b.commitHints[bucket] = h
	return h
}

func (b *memBackend) Close() error { return nil }

func (b *memBackend) rawGet(key []byte) ([]byte, error) {
	bucket := memTreeKey(key)
	t, ok := b.trees[bucket]
	if !ok {
		return nil, ErrNotFound
	}
	item, ok := t.Get(memItem{key: string(key)})
	if !ok {
		return nil, ErrNotFound
	}
	return item.value, nil
}

func (b *memBackend) rawHas(key []byte) (bool, error) {
	bucket := memTreeKey(key)
	t, ok := b.trees[bucket]
	if !ok {
		return false, nil
	}
	_, ok = t.Get(memItem{key: string(key)})
	return ok, nil
}

func (b *memBackend) rawIter(lower, upper []byte) (Iter, error) {
	mi := &memIter{backend: b}
	mi.SetBounds(lower, upper)
	return mi, nil
}

func (b *memBackend) commit(overlay map[string]overlayEntry, _ bool) error {
	for k, e := range overlay {
		bucket := memTreeKeyFromString(k)
		t := b.getTree(bucket)
		if e.tombstone {
			t.Delete(memItem{key: k})
			continue
		}
		t.Set(memItem{key: k, value: e.value})
	}
	return nil
}

func (b *memBackend) commitDirty(overlay map[string]overlayEntry, dirtyKeys []string, _ bool) error {
	keys := append(b.commitBuf[:0], dirtyKeys...)
	sort.Strings(keys)
	b.commitBuf = keys

	prev := ""
	first := true
	for _, k := range keys {
		if !first && k == prev {
			continue
		}
		first = false
		prev = k
		e := overlay[k]
		bucket := memTreeKeyFromString(k)
		t := b.getTree(bucket)
		hint := b.getHint(bucket)
		if e.tombstone {
			t.DeleteHint(memItem{key: k}, hint)
			continue
		}
		t.SetHint(memItem{key: k, value: e.value}, hint)
	}
	return nil
}

type memIter struct {
	// backend lets SetBounds rebind the iter to whichever per-table
	// tree matches the new lower-bound bucket. Without this, a Session
	// that calls Range(table=A) followed by Range(table=B) would see
	// the second range scan the wrong tree because Session pools the
	// iterator across calls (iter.SetBounds rebinds bounds but not
	// the underlying tree).
	backend      *memBackend
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
	m.lower = string(lower)
	m.upper = string(upper)
	m.valid = false
	if m.backend != nil {
		m.tree = m.backend.trees[memTreeKey(lower)] // may be nil for empty table
	}
}

func (m *memIter) First() bool {
	if m.tree == nil {
		m.valid = false
		return false
	}
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
