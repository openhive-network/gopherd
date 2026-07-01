package persiststore

import "sort"

// keyIndex is the in-RAM key→loc index for a Store. It replaces a
// map[string]loc to BOUND and shrink the resident footprint of a huge cold
// tier: instead of storing every (long) key string in the heap, it stores only
// a 64-bit hash + the table byte + the loc, and VERIFIES a probe by reading the
// key back from the append-only data record (where the full key already lives).
//
// Why this matters for the 100M replay: the cold tier holds hundreds of
// millions of comment/vote rows whose keys are long strings (voter+author+
// permlink). A map[string]loc kept every one of those strings resident
// (~100-150 B/entry) AND — being pointer-rich — forced the GC to scan them all
// every cycle. This table is a single pointer-free []idxSlot, so it is ~4x
// smaller AND invisible to the GC scan. Iteration that needs the actual key
// reads it from the mmap'd record (rare/cold paths); point ops (Get/Set/Delete)
// and the fast-restart CountTable stay off the data pages.
//
// Open addressing with linear probing + backward-shift deletion (no tombstones,
// so a long-running store never accumulates delete cruft). Not safe for
// concurrent use (single apply goroutine, like the rest of the chain layer).

// idxSlot is one hash-table slot. used==false marks an empty slot (h==0 is a
// legal hash, so occupancy needs its own flag). Pointer-free by construction:
// the GC never scans the slot array.
type idxSlot struct {
	h    uint64 // hash(key)
	loc  loc    // record location (see persiststore.go)
	tbl  byte   // key[0] (table byte) — lets CountTable stay index-only
	used bool
}

// keyIndex is a flat open-addressing hash table from key→loc.
type keyIndex struct {
	slots []idxSlot
	mask  uint64 // len(slots)-1; len is always a power of two
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

func newKeyIndex(capHint int, keyAt func(recOff uint64) []byte) *keyIndex {
	n := 16
	// size so capHint fits under the 0.7 grow threshold without resizing.
	for n*7 < capHint*10 {
		n <<= 1
	}
	return &keyIndex{slots: make([]idxSlot, n), mask: uint64(n - 1), keyAt: keyAt}
}

func (ix *keyIndex) len() int { return ix.count }

// reset clears the table (used by rebuildByScan / loadSidecar before refilling).
func (ix *keyIndex) reset(capHint int) {
	n := 16
	for n*7 < capHint*10 {
		n <<= 1
	}
	ix.slots = make([]idxSlot, n)
	ix.mask = uint64(n - 1)
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

func (ix *keyIndex) grow() {
	old := ix.slots
	n := len(old) << 1
	ix.slots = make([]idxSlot, n)
	ix.mask = uint64(n - 1)
	// Re-place every live slot by hash alone (no key verification needed: the
	// source table already guarantees uniqueness).
	for i := range old {
		s := old[i]
		if !s.used {
			continue
		}
		pos := s.h & ix.mask
		for ix.slots[pos].used {
			pos = (pos + 1) & ix.mask
		}
		ix.slots[pos] = s
	}
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
