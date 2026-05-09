package state

import (
	"sort"
	"unsafe"

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

// rangeAll walks every per-table btree in ascending bucket order, then
// every key within that bucket in ascending order. Used by the snapshot
// saver. Errors from fn are propagated.
func (b *memBackend) rangeAll(fn func(key, value []byte) error) error {
	buckets := make([]uint32, 0, len(b.trees))
	for k := range b.trees {
		buckets = append(buckets, k)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i] < buckets[j] })
	for _, bk := range buckets {
		t := b.trees[bk]
		var stop error
		t.Scan(func(it memItem) bool {
			if err := fn([]byte(it.key), it.value); err != nil {
				stop = err
				return false
			}
			return true
		})
		if stop != nil {
			return stop
		}
	}
	return nil
}

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
	itBucket     uint32 // bucket the current `it` is bound to
	lower, upper string
	valid        bool
	hasIter      bool
}

func (m *memIter) SetBounds(lower, upper []byte) {
	m.lower = string(lower)
	m.upper = string(upper)
	m.valid = false
	if m.backend == nil {
		m.tree = nil
		return
	}
	bucket := memTreeKey(lower)
	m.tree = m.backend.trees[bucket]
	// If the bucket changed, drop the existing iter so First()
	// allocates a fresh one bound to the new tree. Same-bucket reuse
	// (the common case for hooks that range the same table on every
	// block) lets Seek reuse the IterG's stack across calls instead
	// of growing a fresh slice each time.
	if m.hasIter && m.itBucket != bucket {
		m.it.Release()
		m.hasIter = false
	}
}

func (m *memIter) First() bool {
	if m.tree == nil {
		m.valid = false
		return false
	}
	bucket := memTreeKey([]byte(m.lower))
	if !m.hasIter || m.itBucket != bucket {
		if m.hasIter {
			m.it.Release()
		}
		m.it = m.tree.Iter()
		m.hasIter = true
		m.itBucket = bucket
	}
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

// Key returns the iter's current key as a []byte aliasing the
// underlying btree item's string. Callers MUST treat the result as
// read-only (mutating the backing string is undefined behavior). The
// alias avoids a per-step []byte conversion alloc that contributed
// 1.7GB across a 200K-block window in the cliff region.
func (m *memIter) Key() []byte {
	s := m.it.Item().key
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}
func (m *memIter) ValueAndErr() ([]byte, error) { return m.it.Item().value, nil }
func (m *memIter) Error() error                 { return nil }
func (m *memIter) Close() error {
	if m.hasIter {
		m.it.Release()
		m.hasIter = false
	}
	return nil
}
