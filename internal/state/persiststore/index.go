package persiststore

import (
	"fmt"
	"os"
	"sort"
	"unsafe"

	"golang.org/x/sys/unix"
)

// keyIndex is the key→loc index for a Store. It replaces a map[string]loc to
// BOUND and shrink the resident footprint of a huge cold tier: instead of
// storing every (long) key string, it stores only a 64-bit hash + the table
// byte + the loc, and VERIFIES a probe by reading the key back from the
// append-only data record (where the full key already lives).
//
// The slot array is MMAP-BACKED (a file, index.dat, alongside the data file),
// NOT a Go heap slice. That is the whole point: at 300M cold rows the index is
// ~20 GB; on the Go heap that RAM is permanently resident and un-pageable (the
// "uses 30 GB where hived uses 6-12 GB" bug). Backed by a MAP_SHARED file it
// lives on disk like chainbase's mmap index — the OS keeps only the working set
// resident and pages the rest to disk under memory pressure. idxSlot is
// pointer-free by construction, so aliasing the mapping as []idxSlot is safe
// (the GC never scans it) and every point op (Get/Set/Delete) and CountTable
// stays off the DATA pages.
//
// Open addressing with linear probing + backward-shift deletion (no tombstones,
// so a long-running store never accumulates delete cruft). Not safe for
// concurrent use (single apply goroutine, like the rest of the chain layer).

// idxSlot is one hash-table slot. used==false marks an empty slot (h==0 is a
// legal hash, so occupancy needs its own flag). Pointer-free by construction:
// the GC never scans the slot array, and it can be aliased over an mmap.
type idxSlot struct {
	h    uint64 // hash(key)
	loc  loc    // record location (see persiststore.go)
	tbl  byte   // key[0] (table byte) — lets CountTable stay index-only
	used bool
}

const idxSlotSize = int(unsafe.Sizeof(idxSlot{}))

// keyIndex is a flat open-addressing hash table from key→loc, backed by a
// MAP_SHARED file so the slot array is pageable to disk, not heap-resident.
type keyIndex struct {
	slots []idxSlot // aliases the mmap `data` — NOT independently allocated
	data  []byte    // the MAP_SHARED mapping backing slots
	f     *os.File  // index.dat
	path  string    // index file path (index.dat, or index.dat.compact for Compact)
	mask  uint64    // len(slots)-1; len is always a power of two
	count int
	// keyAt returns the key bytes of the record at recOff, read zero-copy from
	// the data mapping. Used to verify a hash probe against the real key.
	keyAt func(recOff uint64) []byte
}

// hashKey is FNV-1a/64 — deterministic (no per-process seed), cheap, and good
// enough dispersion for these keys. Determinism is irrelevant to consensus (the
// index is never persisted as hashes; the sidecar stores keys), but a fixed
// hash keeps behavior reproducible run to run.
func hashKey(b string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(b); i++ {
		h ^= uint64(b[i])
		h *= 1099511628211
	}
	return h
}

// powTwoFor returns the smallest power of two >= 16 that holds capHint entries
// under the 0.7 grow threshold.
func powTwoFor(capHint int) int {
	n := 16
	for n*7 < capHint*10 {
		n <<= 1
	}
	return n
}

func newKeyIndex(path string, capHint int, keyAt func(recOff uint64) []byte) *keyIndex {
	ix := &keyIndex{path: path, keyAt: keyAt}
	ix.mapSlots(powTwoFor(capHint))
	return ix
}

// mapSlots (re)creates ix.path as a ZEROED file of n slots and mmaps it,
// aliasing ix.slots over the mapping. n must be a power of two. Truncate-to-0
// then truncate-up guarantees the slots start all-zero (used==false) even when
// reusing an existing file. File-backed MAP_SHARED → pageable to disk.
func (ix *keyIndex) mapSlots(n int) {
	if ix.data != nil {
		_ = unix.Munmap(ix.data)
		ix.data = nil
	}
	if ix.f == nil {
		f, err := os.OpenFile(ix.path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			panic(fmt.Sprintf("persiststore: open index %s: %v", ix.path, err))
		}
		ix.f = f
	}
	nbytes := int64(n) * int64(idxSlotSize)
	if err := ix.f.Truncate(0); err != nil { // discard any prior content
		panic(fmt.Sprintf("persiststore: truncate index 0: %v", err))
	}
	if err := ix.f.Truncate(nbytes); err != nil { // grow back, zero-filled
		panic(fmt.Sprintf("persiststore: truncate index %d: %v", nbytes, err))
	}
	data, err := unix.Mmap(int(ix.f.Fd()), 0, int(nbytes), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		panic(fmt.Sprintf("persiststore: mmap index: %v", err))
	}
	ix.data = data
	ix.slots = unsafe.Slice((*idxSlot)(unsafe.Pointer(&data[0])), n)
	ix.mask = uint64(n - 1)
}

// Close unmaps the index and removes its scratch file (the durable index is the
// sidecar; index.dat is rebuilt from it on the next Open).
func (ix *keyIndex) Close() {
	if ix.data != nil {
		_ = unix.Munmap(ix.data)
		ix.data = nil
	}
	if ix.f != nil {
		_ = ix.f.Close()
		ix.f = nil
	}
	if ix.path != "" {
		_ = os.Remove(ix.path)
	}
	ix.slots = nil
}

func (ix *keyIndex) len() int { return ix.count }

// reset clears the table (used by rebuildByScan / loadSidecar before refilling).
func (ix *keyIndex) reset(capHint int) {
	ix.mapSlots(powTwoFor(capHint))
	ix.count = 0
}

// findSlot returns the slot position holding key (matchOK=true), or the first
// empty slot where it would be inserted (matchOK=false). A hash match is
// verified against the real key via keyAt to defend against 64-bit collisions
// (a collision must never silently overwrite a distinct key — that would lose
// state and break consensus).
func (ix *keyIndex) findSlot(key string, h uint64) (pos uint64, matchOK bool) {
	pos = h & ix.mask
	for {
		s := &ix.slots[pos]
		if !s.used {
			return pos, false
		}
		if s.h == h && string(ix.keyAt(s.loc.recOff)) == key {
			return pos, true
		}
		pos = (pos + 1) & ix.mask
	}
}

func (ix *keyIndex) get(key string) (loc, bool) {
	pos, ok := ix.findSlot(key, hashKey(key))
	if !ok {
		return loc{}, false
	}
	return ix.slots[pos].loc, true
}

func (ix *keyIndex) has(key string) bool {
	_, ok := ix.findSlot(key, hashKey(key))
	return ok
}

// set inserts or overwrites key→l. tbl must be key[0] (or 0 for an empty key).
func (ix *keyIndex) set(key string, l loc, tbl byte) {
	if (ix.count+1)*10 >= len(ix.slots)*7 {
		ix.grow()
	}
	h := hashKey(key)
	pos, ok := ix.findSlot(key, h)
	if ok {
		ix.slots[pos].loc = l
		return
	}
	ix.slots[pos] = idxSlot{h: h, loc: l, tbl: tbl, used: true}
	ix.count++
}

// setUnique inserts key→l WITHOUT verifying against existing keys. Only valid
// when the caller guarantees key is not already present (bulk rebuild from a
// set of already-distinct keys, e.g. Compact) — it probes by hash to the first
// empty slot, so it never calls keyAt and is safe to use while the data mapping
// it will eventually point into is not yet the Store's live mapping.
func (ix *keyIndex) setUnique(key string, l loc, tbl byte) {
	if (ix.count+1)*10 >= len(ix.slots)*7 {
		ix.grow()
	}
	h := hashKey(key)
	pos := h & ix.mask
	for ix.slots[pos].used {
		pos = (pos + 1) & ix.mask
	}
	ix.slots[pos] = idxSlot{h: h, loc: l, tbl: tbl, used: true}
	ix.count++
}

// grow doubles the table. It mmaps a fresh (bigger) file, rehashes every live
// slot into it by hash alone (the source table already guarantees uniqueness),
// then atomically renames it over index.dat. Old + new are both mapped only for
// the duration of the rehash; both are file-backed (pageable), so the transient
// is disk, not RSS.
func (ix *keyIndex) grow() {
	oldSlots := ix.slots
	oldData := ix.data
	oldF := ix.f
	n := len(oldSlots) << 1
	newMask := uint64(n - 1)

	growPath := ix.path + ".grow"
	nf, err := os.OpenFile(growPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		panic(fmt.Sprintf("persiststore: open grow index: %v", err))
	}
	nbytes := int64(n) * int64(idxSlotSize)
	if err := nf.Truncate(nbytes); err != nil {
		panic(fmt.Sprintf("persiststore: truncate grow index: %v", err))
	}
	ndata, err := unix.Mmap(int(nf.Fd()), 0, int(nbytes), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		panic(fmt.Sprintf("persiststore: mmap grow index: %v", err))
	}
	nslots := unsafe.Slice((*idxSlot)(unsafe.Pointer(&ndata[0])), n)

	for i := range oldSlots {
		s := oldSlots[i]
		if !s.used {
			continue
		}
		pos := s.h & newMask
		for nslots[pos].used {
			pos = (pos + 1) & newMask
		}
		nslots[pos] = s
	}

	// Retire the old mapping/file and put the new file in its place. The mmap is
	// backed by nf's inode, so renaming the path keeps the mapping valid.
	_ = unix.Munmap(oldData)
	_ = oldF.Close()
	if err := os.Rename(growPath, ix.path); err != nil {
		panic(fmt.Sprintf("persiststore: rename grow index: %v", err))
	}
	ix.data = ndata
	ix.slots = nslots
	ix.mask = newMask
	ix.f = nf
}

// del removes key (backward-shift deletion to keep probe chains contiguous
// without tombstones). `gap` is the freed slot; `j` scans forward. A scanned
// slot is moved back into the gap iff the gap is reachable from its home
// (i.e. its home is NOT in the open interval (gap, j]); otherwise it is
// "anchored" and left in place while the gap stays put. The gap is cleared once
// an empty slot ends the chain.
func (ix *keyIndex) del(key string) {
	gap, ok := ix.findSlot(key, hashKey(key))
	if !ok {
		return
	}
	ix.count--
	j := gap
	for {
		j = (j + 1) & ix.mask
		if !ix.slots[j].used {
			break
		}
		home := ix.slots[j].h & ix.mask
		if cyclicIn(home, gap, j) {
			continue // anchored: leave it, keep the gap, advance the scan
		}
		ix.slots[gap] = ix.slots[j]
		gap = j
	}
	ix.slots[gap] = idxSlot{}
}

// cyclicIn reports whether x is in the cyclic interval (lo, hi] on the ring of
// size len(slots). Used by backward-shift deletion to decide if a slot is
// "anchored" between the gap and its current position.
func cyclicIn(x, lo, hi uint64) bool {
	if lo < hi {
		return x > lo && x <= hi
	}
	return x > lo || x <= hi
}

// eachTable calls fn(tbl, loc) for every live slot — INDEX ONLY (no data-page
// faults), so CountTable/RangeTable accounting stays cheap on a huge cold tier.
func (ix *keyIndex) eachTable(fn func(tbl byte, l loc) bool) {
	for i := range ix.slots {
		if ix.slots[i].used {
			if !fn(ix.slots[i].tbl, ix.slots[i].loc) {
				return
			}
		}
	}
}

// eachKey calls fn(key, loc) for every live slot, reading each key back from the
// data mapping (faults cold pages — for the rarer key-iterating paths: Flush,
// Compact, SortedKeys, prefix listing, by-offset snapshot scan).
func (ix *keyIndex) eachKey(fn func(key []byte, l loc) bool) {
	// Read records in FILE-OFFSET order (sequential), not hash-slot order
	// (random). On a cold tier bigger than page cache, hash-order key reads
	// fault the file randomly — that thrashed page cache and made Flush hang
	// 60min+ and persist resume-load crawl. Sorting a compact []uint32 of live
	// slot indices by recOff is RAM-only (no data faults); the keyAt reads then
	// march up the file. Every eachKey caller is a cold path (load / flush /
	// snapshot-save / compaction), never the per-block apply loop.
	order := make([]uint32, 0, ix.count)
	for i := range ix.slots {
		if ix.slots[i].used {
			order = append(order, uint32(i))
		}
	}
	sort.Slice(order, func(a, b int) bool {
		return ix.slots[order[a]].loc.recOff < ix.slots[order[b]].loc.recOff
	})
	for _, si := range order {
		if !fn(ix.keyAt(ix.slots[si].loc.recOff), ix.slots[si].loc) {
			return
		}
	}
}
