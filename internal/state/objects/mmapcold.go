package objects

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"unsafe"

	"github.com/tidwall/btree"
	"golang.org/x/sys/unix"
)

// coldStore is the cold-tier backend interface. Two implementations exist:
// coldKV (pebble LSM, battle-tested) and mmapColdKV (mmap file + btree index,
// faster for the replay's write-heavy/read-rare eviction pattern). The
// comment/vote stores hold a coldStore so the backend is swappable.
type coldStore interface {
	bset(key, val []byte)
	bdel(key []byte)
	flushBatch()
	get(key []byte) ([]byte, bool)
	has(key []byte) bool
	set(key, val []byte) error
	del(key []byte) error
	deletePrefix(prefix []byte) error
	rangePrefix(prefix []byte, fn func(key, val []byte) bool) error
	// rangePrimaryByOffset streams the VALUES of every primary-table key under
	// prefix, in ASCENDING FILE-OFFSET order, so the underlying blob reads are
	// SEQUENTIAL (no random mmap page-fault amplification). The snapshot save
	// uses this: map-order reads thrashed at ~6 MB/s (57 MB/s read-amplified)
	// reserializing tens of GB of cold rows; offset order is disk-sequential.
	rangePrimaryByOffset(prefix []byte, fn func(val []byte) bool) error
	newPrefixIter(prefix []byte) (coldIterator, error)
	Dir() string
	Close() error
}

// coldIterator is a pull-style ascending iterator over a key prefix,
// positioned ON the first matching key. Used by the snapshot merge.
type coldIterator interface {
	valid() bool
	key() []byte
	val() ([]byte, error)
	next()
	Close() error
}

// --- mmap-backed implementation ---------------------------------------------

// mmapColdFileSize reserves a large SPARSE virtual mapping; only touched
// pages consume disk/RAM (page cache). No remap on growth → cold read slices
// into the mapping stay valid for the process lifetime. Sized for the dense
// era; a full 60M replay (~150GB cold + dead space) needs reclaim or a larger
// reservation (TODO).
// 400 GiB sparse reservation. Only touched pages consume disk; a full 60M
// replay's cold data is ~150-200 GB plus promote/re-evict dead space (no
// reclaim), so 220 GiB risked a mid-run "data file full". 400 GiB removes that
// (disk has ~700 GB free); the bset panic is the backstop if it's ever hit.
const mmapColdFileSize = 400 << 30

type mmapColdEntry struct {
	key    []byte
	offset uint64
	length uint32
}

func mmapColdLess(a, b mmapColdEntry) bool { return bytes.Compare(a.key, b.key) < 0 }

// mmapPrimVal is a primary-table index value: the blob's location in the
// mapping. The map KEY is an unsafe.String aliasing the key bytes already
// stored in the mapping — zero-copy (no per-evict key alloc) and GC-cheap (the
// header points outside the Go heap, so scanObject skips it).
type mmapPrimVal struct {
	offset uint64
	length uint32
}

// coldKeyIsPrimary routes hot point-op tables (comment/vote primary) to the
// O(1) hash map; the by-root/by-comment tables (cashout prefix scans only)
// stay in the ordered btree. Routing by the table prefix byte (key[0]).
func coldKeyIsPrimary(key []byte) bool {
	return len(key) > 0 && (key[0] == coldTableCommentPrimary || key[0] == coldTableVotePrimary)
}

type mmapColdKV struct {
	dir      string
	owned    bool
	file     *os.File
	data     []byte // the mmap (length == mmapColdFileSize)
	writeOff uint64
	prim     map[string]mmapPrimVal // primary tables: O(1) point ops, mmap-aliased keys
	idx      *btree.BTreeG[mmapColdEntry]
}

// OpenColdTier opens the configured cold-tier backend (pebble by default,
// mmap when useMmap). Returned value is used through the coldStore interface.
func OpenColdTier(dir string, useMmap bool) (coldStore, error) {
	if useMmap {
		return openMmapColdKV(dir)
	}
	return openColdKV(dir)
}

// OpenColdTierBackend selects the cold-tier backend by name:
//
//	"mmap"    — mmapColdKV (volatile: O_TRUNC on open → lost on restart)
//	"pebble"  — coldKV LSM (volatile across our use)
//	"persist" — PersistCold (chainbase-in-Go: SURVIVES restart, no replay)
//
// "persist" is the cutover backend: same mmap speed as "mmap" but append-only +
// index-restored, so the cold tier (the bulk of state) comes back at head.
func OpenColdTierBackend(dir, backend string) (coldStore, error) {
	switch backend {
	case "", "mmap":
		return openMmapColdKV(dir)
	case "pebble":
		return openColdKV(dir)
	case "persist":
		return OpenPersistCold(dir)
	default:
		return nil, fmt.Errorf("unknown cold-tier backend %q (want mmap|pebble|persist)", backend)
	}
}

func openMmapColdKV(dir string) (*mmapColdKV, error) {
	owned := dir == ""
	if owned {
		var err error
		dir, err = os.MkdirTemp("", "ghive-mmapcold-")
		if err != nil {
			return nil, err
		}
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "cold.dat"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(mmapColdFileSize); err != nil {
		f.Close()
		return nil, fmt.Errorf("mmapcold: truncate: %w", err)
	}
	data, err := unix.Mmap(int(f.Fd()), 0, mmapColdFileSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmapcold: mmap: %w", err)
	}
	// Random access on promote reads; the kernel needn't read-ahead.
	_ = unix.Madvise(data, unix.MADV_RANDOM)
	return &mmapColdKV{
		dir:   dir,
		owned: owned,
		file:  f,
		data:  data,
		prim:  make(map[string]mmapPrimVal, 1<<21),
		idx:   btree.NewBTreeGOptions[mmapColdEntry](mmapColdLess, btree.Options{Degree: 32, NoLocks: true}),
	}, nil
}

func (c *mmapColdKV) Dir() string { return c.dir }

func (c *mmapColdKV) set(key, val []byte) error {
	need := c.writeOff + uint64(len(key)) + uint64(len(val))
	if need > uint64(len(c.data)) {
		return fmt.Errorf("mmapcold: data file full (%d bytes, need %d)", len(c.data), need)
	}
	// Write key THEN val inline; the btree entry's key field is a zero-copy
	// slice into the mapping (no per-evict key alloc — the dominant remaining
	// GC source). The mapping is persistent so the slice stays valid; btree
	// rebalancing only copies the slice header. Old key+blob on re-set/del
	// become dead space (bounded: cold comments are promoted before mutation).
	off := c.writeOff
	copy(c.data[off:], key)
	kEnd := off + uint64(len(key))
	copy(c.data[kEnd:], val)
	c.writeOff = need
	if coldKeyIsPrimary(key) {
		// Map key aliases the just-written key bytes in the mapping (no copy).
		keyStr := unsafe.String(&c.data[off], len(key))
		c.prim[keyStr] = mmapPrimVal{offset: kEnd, length: uint32(len(val))}
		return nil
	}
	keySlice := c.data[off:kEnd:kEnd]
	c.idx.Set(mmapColdEntry{key: keySlice, offset: kEnd, length: uint32(len(val))})
	return nil
}

func (c *mmapColdKV) bset(key, val []byte) {
	// A cold-tier write MUST NOT silently fail: Evict removes the row from RAM
	// regardless, so a swallowed "data file full" error loses the record
	// entirely → silent consensus divergence + a snapshot count-header that
	// over-counts (truncated-read EOF on later load). Panic loudly instead: the
	// process exits non-zero, the watchdog resumes from the last snapshot, and
	// the cause is visible. The reservation (mmapColdFileSize) is sized so this
	// never fires on a 60M replay; the panic is a correctness backstop only.
	if err := c.set(key, val); err != nil {
		panic(fmt.Sprintf("mmapcold: %v — raise mmapColdFileSize or add cold-tier compaction", err))
	}
}

func (c *mmapColdKV) del(key []byte) error {
	if coldKeyIsPrimary(key) {
		delete(c.prim, string(key))
		return nil
	}
	c.idx.Delete(mmapColdEntry{key: key})
	return nil
}

func (c *mmapColdKV) bdel(key []byte) { _ = c.del(key) }

// flushBatch is a no-op: mmap writes are immediately visible (no batching).
func (c *mmapColdKV) flushBatch() {}

func (c *mmapColdKV) get(key []byte) ([]byte, bool) {
	if coldKeyIsPrimary(key) {
		// string(key) in a map index is compiler-optimized — no alloc.
		e, ok := c.prim[string(key)]
		if !ok {
			return nil, false
		}
		end := e.offset + uint64(e.length)
		return c.data[e.offset:end:end], true
	}
	e, ok := c.idx.Get(mmapColdEntry{key: key})
	if !ok {
		return nil, false
	}
	end := e.offset + uint64(e.length)
	// Zero-copy slice into the page-cached mapping; capped so a caller can't
	// append into the next record. Valid for the process lifetime (no remap).
	return c.data[e.offset:end:end], true
}

func (c *mmapColdKV) has(key []byte) bool {
	if coldKeyIsPrimary(key) {
		_, ok := c.prim[string(key)]
		return ok
	}
	_, ok := c.idx.Get(mmapColdEntry{key: key})
	return ok
}

func (c *mmapColdKV) deletePrefix(prefix []byte) error {
	upper := prefixUpperBound(prefix)
	var keys [][]byte
	c.idx.Ascend(mmapColdEntry{key: prefix}, func(e mmapColdEntry) bool {
		if upper != nil && bytes.Compare(e.key, upper) >= 0 {
			return false
		}
		keys = append(keys, e.key)
		return true
	})
	for _, k := range keys {
		c.idx.Delete(mmapColdEntry{key: k})
	}
	return nil
}

func (c *mmapColdKV) rangePrefix(prefix []byte, fn func(key, val []byte) bool) error {
	upper := prefixUpperBound(prefix)
	// Primary-table keys live in the `prim` hash map, not the btree. STREAM them
	// directly (no sort, no materialized slice) — the snapshot save iterates
	// tens of millions of cold comments here; collecting+sorting them all in
	// memory (the old newPrefixIter path) blew the heap near GOMEMLIMIT and
	// hung the save in a GC spiral. Map order is unspecified, which is fine:
	// snapshot load is order-independent.
	if coldKeyIsPrimary(prefix) {
		for k, v := range c.prim {
			kb := unsafe.Slice(unsafe.StringData(k), len(k))
			if bytes.Compare(kb, prefix) < 0 {
				continue
			}
			if upper != nil && bytes.Compare(kb, upper) >= 0 {
				continue
			}
			end := v.offset + uint64(v.length)
			if !fn(kb, c.data[v.offset:end:end]) {
				return nil
			}
		}
		return nil
	}
	c.idx.Ascend(mmapColdEntry{key: prefix}, func(e mmapColdEntry) bool {
		if upper != nil && bytes.Compare(e.key, upper) >= 0 {
			return false
		}
		end := e.offset + uint64(e.length)
		return fn(e.key, c.data[e.offset:end:end])
	})
	return nil
}

// rangePrimaryByOffset streams primary values in ascending file-offset order.
// Collects a PACKED (offset<<24 | length) uint64 per matching entry — 8 bytes
// each, half the old materialize — sorts (offset is the high bits, so a plain
// uint64 sort orders by offset), then reads each blob sequentially from the
// mmap. Sequential reads share pages with adjacent records → no fault
// amplification → disk-bandwidth speed instead of the ~6 MB/s random thrash.
func (c *mmapColdKV) rangePrimaryByOffset(prefix []byte, fn func(val []byte) bool) error {
	if !coldKeyIsPrimary(prefix) {
		// by-root/by-comment (btree) prefixes are already key-ordered and small
		// per-scan; just delegate.
		return c.rangePrefix(prefix, func(_, v []byte) bool { return fn(v) })
	}
	upper := prefixUpperBound(prefix)
	packed := make([]uint64, 0, len(c.prim))
	for k, v := range c.prim {
		kb := unsafe.Slice(unsafe.StringData(k), len(k))
		if bytes.Compare(kb, prefix) < 0 {
			continue
		}
		if upper != nil && bytes.Compare(kb, upper) >= 0 {
			continue
		}
		// offset < 2^40 (file ≤ 400 GiB), length < 2^24 (16 MB ≫ any blob).
		packed = append(packed, v.offset<<24|uint64(v.length))
	}
	sort.Slice(packed, func(i, j int) bool { return packed[i] < packed[j] })
	// We now read blobs in ascending offset order. mmap page faults do NOT
	// trigger readahead by default, so even offset-ordered reads were random
	// 4KB faults (~52 MB/s) — the snapshot-save bottleneck. MADV_SEQUENTIAL
	// tells the kernel to prefetch aggressively for this scan → disk-sequential
	// bandwidth. Restore MADV_NORMAL after: the cold tier's steady-state access
	// (point gets on promote) is random, where sequential readahead would waste
	// I/O. The scan runs only at a snapshot save (replay paused), so toggling
	// the advice here is safe.
	if len(c.data) > 0 {
		_ = unix.Madvise(c.data, unix.MADV_SEQUENTIAL)
		defer func() { _ = unix.Madvise(c.data, unix.MADV_NORMAL) }()
	}
	for _, p := range packed {
		off := p >> 24
		ln := uint64(p & 0xFFFFFF)
		end := off + ln
		if !fn(c.data[off:end:end]) {
			return nil
		}
	}
	return nil
}

// mmapColdIter materializes the matching index ENTRIES (keys+offsets, not the
// values — those are read lazily zero-copy from the mmap on val()). For
// RangeByRoot/RangeByComment the matching set is one comment/root (small). The
// only full-scan caller is the snapshot merge (deferred/unused in the perf
// path); a streaming pull-iterator is a TODO for when snapshots are wired.
type mmapColdIter struct {
	ents []mmapColdEntry
	data []byte
	pos  int
}

func (c *mmapColdKV) newPrefixIter(prefix []byte) (coldIterator, error) {
	upper := prefixUpperBound(prefix)
	mi := &mmapColdIter{data: c.data}
	// Primary-table keys (comment/vote primary) live in the `prim` hash map,
	// NOT the btree (the O(1) point-op routing). A prefix scan over a primary
	// table — the snapshot's full primary merge — must therefore enumerate the
	// map, sorted, instead of the (by-root-only) btree. Without this the
	// snapshot silently omitted every cold-tier comment (header counted them,
	// body didn't write them → truncated-read EOF on load).
	if coldKeyIsPrimary(prefix) {
		for k, v := range c.prim {
			kb := unsafe.Slice(unsafe.StringData(k), len(k))
			if bytes.Compare(kb, prefix) < 0 {
				continue
			}
			if upper != nil && bytes.Compare(kb, upper) >= 0 {
				continue
			}
			mi.ents = append(mi.ents, mmapColdEntry{key: kb, offset: v.offset, length: v.length})
		}
		sort.Slice(mi.ents, func(i, j int) bool {
			return bytes.Compare(mi.ents[i].key, mi.ents[j].key) < 0
		})
		return mi, nil
	}
	c.idx.Ascend(mmapColdEntry{key: prefix}, func(e mmapColdEntry) bool {
		if upper != nil && bytes.Compare(e.key, upper) >= 0 {
			return false
		}
		mi.ents = append(mi.ents, e)
		return true
	})
	return mi, nil
}

func (m *mmapColdIter) valid() bool { return m.pos < len(m.ents) }
func (m *mmapColdIter) key() []byte { return m.ents[m.pos].key }
func (m *mmapColdIter) val() ([]byte, error) {
	e := m.ents[m.pos]
	end := e.offset + uint64(e.length)
	return m.data[e.offset:end:end], nil
}
func (m *mmapColdIter) next()        { m.pos++ }
func (m *mmapColdIter) Close() error { return nil }

func (c *mmapColdKV) Close() error {
	if c == nil {
		return nil
	}
	if c.data != nil {
		_ = unix.Munmap(c.data)
		c.data = nil
	}
	if c.file != nil {
		_ = c.file.Close()
		c.file = nil
	}
	if c.owned && c.dir != "" {
		_ = os.RemoveAll(c.dir)
	}
	return nil
}
