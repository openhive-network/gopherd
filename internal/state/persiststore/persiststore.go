// Package persiststore is the foundational primitive for chainbase-in-Go (see
// DESIGN.md): an mmap-backed key/value store of self-describing records that
// SURVIVES PROCESS RESTART without replay. Phase 1 — data segment + slot
// allocator (append-only log) + in-RAM index + clean-shutdown
// flush (index sidecar) + open (sidecar-or-scan rebuild).
//
// LOG-STRUCTURED: the data segment is APPEND-ONLY. Set/Delete append a new
// record; superseded records become dead space, reclaimed by Compact(). This
// makes the index rebuildable by a forward scan after a CRASH (highest-offset
// record per key wins; a tombstone means deleted) — so a restart never needs
// replay, even on an unclean kill (stronger than chainbase, which requires
// replay on a dirty shutdown).
//
// Record layout (self-describing):
//
//	[recType 1B][keyLen uvarint][key bytes][valLen uvarint][val bytes]
//
// recType: recLive (0) = key→val; recTomb (1) = key deleted (valLen==0).
//
// Values are read zero-copy as slices into the mapping. Not safe for concurrent
// mutation (single apply goroutine, like the rest of the chain layer).
package persiststore

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/tidwall/btree"
	"golang.org/x/sys/unix"
)

// prefixUpperBound returns the exclusive upper bound for a prefix scan: the
// smallest key strictly greater than every key starting with prefix, or nil if
// prefix is all-0xFF (no upper bound).
func prefixUpperBound(prefix []byte) []byte {
	out := make([]byte, len(prefix))
	copy(out, prefix)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xFF {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}

const (
	headerSize = 4096       // page 0: magic | version | writeOff | dirty
	magic      = 0x50455253 // "PERS"
	version    = 1

	recLive byte = 0
	recTomb byte = 1
)

// loc locates one record's VALUE in the data segment. recOff is the record
// start; valOff/valLen the value; span is the whole record's byte length (used
// to tally dead space for compaction).
type loc struct {
	recOff uint64
	valOff uint64
	valLen uint32
	span   uint32
}

// Store is a persistent, log-structured, mmap-backed record store for one table.
type Store struct {
	dir       string
	dataPath  string
	f         *os.File
	data      []byte // mmap (MAP_SHARED); writes flushed via msync
	size      uint64 // mapping length (sparse reservation)
	writeOff  uint64 // append frontier
	idx       map[string]loc
	liveBytes uint64 // sum of live record spans (dead = writeOff-headerSize-liveBytes)
	dirty     bool

	// Undo (phase 2) — mirrors the typed-store SnapshotPoint/RestoreToPoint/
	// CommitPoint contract so a persiststore-backed table participates in
	// per-transaction rollback like the in-memory stores. Log-structured undo
	// is cheap: a mutation's PRIOR record still lives in the log (only the index
	// moved), so undo just restores the index entry; abandoned records become
	// dead space (reclaimed by Compact; accounted in liveBytes).
	undoEnabled bool
	undoLog     []undoEntry

	// Ordered prefix-scan index (phase 4). Only keys whose first byte is a
	// REGISTERED ordered-table byte are kept in `ordered` (a sorted btree of
	// keys) — so point-op tables (primary keys, the bulk) stay map-only and
	// cheap, while secondary-index tables (e.g. votes-by-comment, comments-by-
	// root) get O(log n) prefix scans without btree'ing every key. Mirrors the
	// cold tier's map(primary)+btree(secondary) split.
	orderedTable [256]bool
	ordered      *btree.BTreeG[string]
}

// undoEntry captures what one Set/Delete changed, so it can be reversed.
type undoEntry struct {
	key          string
	prior        loc    // index loc for key BEFORE this mutation
	priorExisted bool   // false → key was absent before
	addedSpan    uint32 // span of the record this mutation appended
	addedLive    bool   // appended record counted toward liveBytes (Set yes, tombstone no)
}

// Open opens (creating if absent) a persistent store under dir, reserving a
// sparse mapping of reserveBytes. If a clean index sidecar exists it is loaded
// (fast restart); otherwise — fresh, or a dirty/crash shutdown — the index is
// rebuilt by a forward scan of the data segment (no replay needed).
func Open(dir string, reserveBytes uint64) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("persiststore: mkdir: %w", err)
	}
	dataPath := filepath.Join(dir, "data.bin")
	f, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("persiststore: open data: %w", err)
	}
	if err := f.Truncate(int64(reserveBytes)); err != nil {
		f.Close()
		return nil, fmt.Errorf("persiststore: truncate: %w", err)
	}
	data, err := unix.Mmap(int(f.Fd()), 0, int(reserveBytes), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("persiststore: mmap: %w", err)
	}
	s := &Store{
		dir:      dir,
		dataPath: dataPath,
		f:        f,
		data:     data,
		size:     reserveBytes,
		idx:      make(map[string]loc, 1<<16),
		writeOff: headerSize,
	}
	mg := binary.LittleEndian.Uint32(s.data[0:4])
	if mg == magic {
		s.writeOff = binary.LittleEndian.Uint64(s.data[8:16])
		dirtyByte := s.data[16]
		if loadErr := s.loadSidecar(); loadErr != nil || dirtyByte != 0 {
			// No clean sidecar, or a dirty (crash) shutdown → rebuild by scan.
			if err := s.rebuildByScan(); err != nil {
				s.Close()
				return nil, err
			}
		}
	} else {
		// Fresh file: write the header.
		binary.LittleEndian.PutUint32(s.data[0:4], magic)
		binary.LittleEndian.PutUint32(s.data[4:8], version)
		binary.LittleEndian.PutUint64(s.data[8:16], s.writeOff)
		s.data[16] = 0
	}
	return s, nil
}

func (s *Store) markDirty() {
	if !s.dirty {
		s.dirty = true
		s.data[16] = 1
	}
}

// RegisterOrderedTable marks keys with first byte `table` as needing the ordered
// prefix-scan index. Call once per such table right after Open, before any Set.
// Reseeds the index from existing keys (so it works after a sidecar/scan open).
func (s *Store) RegisterOrderedTable(table byte) {
	s.orderedTable[table] = true
	if s.ordered == nil {
		s.ordered = btree.NewBTreeGOptions[string](
			func(a, b string) bool { return a < b },
			btree.Options{Degree: 32, NoLocks: true},
		)
	}
	for k := range s.idx {
		if len(k) > 0 && s.orderedTable[k[0]] {
			s.ordered.Set(k)
		}
	}
}

func (s *Store) isOrdered(key string) bool {
	return s.ordered != nil && len(key) > 0 && s.orderedTable[key[0]]
}

// RangePrefix calls fn(key, val) for every live key with the given prefix, in
// ascending key order. The key's table byte must have been RegisterOrderedTable'd.
// val is a zero-copy view into the mapping (do not retain across mutations).
func (s *Store) RangePrefix(prefix []byte, fn func(key string, val []byte) bool) {
	if s.ordered == nil {
		return
	}
	upper := prefixUpperBound(prefix)
	s.ordered.Ascend(string(prefix), func(k string) bool {
		if upper != nil && k >= string(upper) {
			return false
		}
		l, ok := s.idx[k]
		if !ok {
			return true
		}
		return fn(k, s.data[l.valOff:l.valOff+uint64(l.valLen):l.valOff+uint64(l.valLen)])
	})
}

// alloc appends a record slot of size `need` at the frontier (log-structured;
// no in-place reuse — superseded slots are reclaimed by Compact).
func (s *Store) alloc(need uint32) (uint64, error) {
	off := s.writeOff
	if off+uint64(need) > s.size {
		return 0, fmt.Errorf("persiststore: data full (size %d, need %d at %d)", s.size, need, off)
	}
	s.writeOff = off + uint64(need)
	binary.LittleEndian.PutUint64(s.data[8:16], s.writeOff)
	return off, nil
}

// recordSpan returns the on-disk byte length of a record holding (klen,vlen):
// 1 type byte + uvarint(klen) + key + uvarint(vlen) + val.
func recordSpan(klen, vlen int) uint32 {
	return uint32(1 + uvarintLen(uint64(klen)) + klen + uvarintLen(uint64(vlen)) + vlen)
}

func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// Set appends a live record for key→val (superseding any prior record).
func (s *Store) Set(key string, val []byte) error {
	s.markDirty()
	span := recordSpan(len(key), len(val))
	prev, existed := s.idx[key]
	if s.undoEnabled {
		s.undoLog = append(s.undoLog, undoEntry{
			key: key, prior: prev, priorExisted: existed, addedSpan: span, addedLive: true,
		})
	}
	if existed {
		s.liveBytes -= uint64(prev.span) // old record is now dead space
	}
	off, err := s.alloc(span)
	if err != nil {
		return err
	}
	p := off
	s.data[p] = recLive
	p++
	n := binary.PutUvarint(s.data[p:], uint64(len(key)))
	p += uint64(n)
	copy(s.data[p:], key)
	p += uint64(len(key))
	n = binary.PutUvarint(s.data[p:], uint64(len(val)))
	p += uint64(n)
	valOff := p
	copy(s.data[p:], val)
	s.idx[key] = loc{recOff: off, valOff: valOff, valLen: uint32(len(val)), span: span}
	s.liveBytes += uint64(span)
	if s.isOrdered(key) {
		s.ordered.Set(key) // idempotent on overwrite
	}
	return nil
}

// Get returns a zero-copy view of key's value (valid until the next Set/flush
// that may reuse the slot). ok=false on miss.
func (s *Store) Get(key string) ([]byte, bool) {
	l, ok := s.idx[key]
	if !ok {
		return nil, false
	}
	return s.data[l.valOff : l.valOff+uint64(l.valLen) : l.valOff+uint64(l.valLen)], true
}

// Delete appends a tombstone for key (so a crash-time scan-rebuild knows the
// key is gone). The prior live record becomes dead space.
func (s *Store) Delete(key string) {
	l, ok := s.idx[key]
	if !ok {
		return
	}
	s.markDirty()
	span := recordSpan(len(key), 0)
	if s.undoEnabled {
		s.undoLog = append(s.undoLog, undoEntry{
			key: key, prior: l, priorExisted: true, addedSpan: span, addedLive: false,
		})
	}
	s.liveBytes -= uint64(l.span)
	delete(s.idx, key)
	if s.isOrdered(key) {
		s.ordered.Delete(key)
	}
	off, err := s.alloc(span)
	if err != nil {
		return // mapping full; the in-RAM delete already took effect
	}
	p := off
	s.data[p] = recTomb
	p++
	n := binary.PutUvarint(s.data[p:], uint64(len(key)))
	p += uint64(n)
	copy(s.data[p:], key)
	// tombstone carries no value (valLen 0); the trailing 0 byte is the vlen.
	p += uint64(len(key))
	s.data[p] = 0
}

// Len returns the number of live keys.
func (s *Store) Len() int { return len(s.idx) }

// SortedKeys returns all live keys in ascending order (for ordered range scans).
func (s *Store) SortedKeys() []string {
	out := make([]string, 0, len(s.idx))
	for k := range s.idx {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SetUndoEnabled turns undo recording on/off (the wrapper mirrors the chain's
// db.UndoEnabled). When off, Set/Delete record no undo entries.
func (s *Store) SetUndoEnabled(b bool) { s.undoEnabled = b }

// SnapshotPoint returns the current undo-log length — the savepoint marker.
func (s *Store) SnapshotPoint() int { return len(s.undoLog) }

// RestoreToPoint reverts every mutation recorded after `snap`, restoring the
// index (and liveBytes) to that point. The abandoned appended records remain in
// the log as dead space (reclaimed by Compact). Mirrors the typed stores.
func (s *Store) RestoreToPoint(snap int) {
	if snap < 0 {
		snap = 0
	}
	if snap > len(s.undoLog) {
		return
	}
	for i := len(s.undoLog) - 1; i >= snap; i-- {
		e := s.undoLog[i]
		if e.addedLive {
			s.liveBytes -= uint64(e.addedSpan) // appended record is abandoned
		}
		if e.priorExisted {
			s.idx[e.key] = e.prior
			s.liveBytes += uint64(e.prior.span)
			if !e.addedLive && s.isOrdered(e.key) {
				s.ordered.Set(e.key) // undo of a Delete → key reappears
			}
		} else {
			delete(s.idx, e.key)
			if s.isOrdered(e.key) {
				s.ordered.Delete(e.key) // undo of a Set that created the key
			}
		}
	}
	s.undoLog = s.undoLog[:snap]
}

// CommitPoint discards the undo log (the appended records are now permanent;
// their superseded predecessors are dead space).
func (s *Store) CommitPoint() { s.undoLog = s.undoLog[:0] }

// rebuildByScan reconstructs the index by walking live + dead records from the
// data frontier. Used after a crash/dirty open. NOTE: dead (freed/superseded)
// records are also scanned; the LAST write per key wins because appends are
// monotonic and a key's live record is always its highest offset. (Freed slots
// reused by later writes are overwritten in place, so a stale earlier record
// for a reused offset cannot exist.) For phase 1 we conservatively keep the
// highest-offset record per key.
func (s *Store) rebuildByScan() error {
	s.idx = make(map[string]loc, 1<<16)
	s.liveBytes = 0
	p := uint64(headerSize)
	for p < s.writeOff {
		recOff := p
		if p >= uint64(len(s.data)) {
			break
		}
		typ := s.data[p]
		p++
		klen, n := binary.Uvarint(s.data[p:])
		if n <= 0 {
			break // unwritten tail
		}
		p += uint64(n)
		key := string(s.data[p : p+klen])
		p += klen
		vlen, n := binary.Uvarint(s.data[p:])
		if n <= 0 {
			return fmt.Errorf("persiststore: scan: bad val len at %d", recOff)
		}
		p += uint64(n)
		valOff := p
		p += vlen
		if p > s.size {
			return fmt.Errorf("persiststore: scan: record overruns mapping at %d", recOff)
		}
		// Records are in ascending offset order, so the LAST record for a key
		// wins: a later live record supersedes; a later tombstone deletes.
		if prev, ok := s.idx[key]; ok {
			s.liveBytes -= uint64(prev.span)
		}
		if typ == recTomb {
			delete(s.idx, key)
			continue
		}
		span := uint32(p - recOff)
		s.idx[key] = loc{recOff: recOff, valOff: valOff, valLen: uint32(vlen), span: span}
		s.liveBytes += uint64(span)
	}
	return nil
}

// sidecarPath is the persisted index (clean-shutdown fast restart).
func (s *Store) sidecarPath() string { return filepath.Join(s.dir, "index.bin") }

// Flush persists everything for a clean restart: msync the data mapping, write
// the index sidecar (sorted keys, deterministic), and clear the dirty flag.
func (s *Store) Flush() error {
	if err := unix.Msync(s.data, unix.MS_SYNC); err != nil {
		return fmt.Errorf("persiststore: msync: %w", err)
	}
	keys := make([]string, 0, len(s.idx))
	for k := range s.idx {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tmp := s.sidecarPath() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	var hdr [16]byte
	binary.LittleEndian.PutUint32(hdr[0:4], magic)
	binary.LittleEndian.PutUint32(hdr[4:8], version)
	binary.LittleEndian.PutUint64(hdr[8:16], uint64(len(keys)))
	w.Write(hdr[:])
	var vb [binary.MaxVarintLen64]byte
	for _, k := range keys {
		l := s.idx[k]
		n := binary.PutUvarint(vb[:], uint64(len(k)))
		w.Write(vb[:n])
		w.WriteString(k)
		n = binary.PutUvarint(vb[:], l.recOff)
		w.Write(vb[:n])
		n = binary.PutUvarint(vb[:], l.valOff)
		w.Write(vb[:n])
		n = binary.PutUvarint(vb[:], uint64(l.valLen))
		w.Write(vb[:n])
		n = binary.PutUvarint(vb[:], uint64(l.span))
		w.Write(vb[:n])
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if err := os.Rename(tmp, s.sidecarPath()); err != nil {
		return err
	}
	s.dirty = false
	s.data[16] = 0
	return unix.Msync(s.data[:headerSize], unix.MS_SYNC)
}

// loadSidecar restores the index from a clean-shutdown sidecar. Returns an error
// if absent/corrupt (caller falls back to rebuildByScan).
func (s *Store) loadSidecar() error {
	b, err := os.ReadFile(s.sidecarPath())
	if err != nil {
		return err
	}
	if len(b) < 16 || binary.LittleEndian.Uint32(b[0:4]) != magic {
		return fmt.Errorf("persiststore: bad sidecar")
	}
	count := binary.LittleEndian.Uint64(b[8:16])
	p := 16
	s.idx = make(map[string]loc, count)
	s.liveBytes = 0
	for i := uint64(0); i < count; i++ {
		klen, n := binary.Uvarint(b[p:])
		if n <= 0 {
			return fmt.Errorf("persiststore: sidecar klen[%d]", i)
		}
		p += n
		key := string(b[p : p+int(klen)])
		p += int(klen)
		recOff, n := binary.Uvarint(b[p:])
		p += n
		valOff, n := binary.Uvarint(b[p:])
		p += n
		valLen, n := binary.Uvarint(b[p:])
		p += n
		span, n := binary.Uvarint(b[p:])
		p += n
		s.idx[key] = loc{recOff: recOff, valOff: valOff, valLen: uint32(valLen), span: uint32(span)}
		s.liveBytes += span
	}
	return nil
}

// DeadRatio is the fraction of the used data segment that is dead (superseded
// records + tombstones). Compaction reclaims it.
func (s *Store) DeadRatio() float64 {
	used := s.writeOff - headerSize
	if used == 0 {
		return 0
	}
	return float64(used-s.liveBytes) / float64(used)
}

// Compact rewrites all live records into a fresh data file in sorted-key order,
// reclaiming every dead byte, then atomically swaps it in and re-mmaps. Safe to
// call only at a quiescent point (no held value slices; replay paused).
func (s *Store) Compact() error {
	keys := make([]string, 0, len(s.idx))
	for k := range s.idx {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	tmpPath := s.dataPath + ".compact"
	nf, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := nf.Truncate(int64(s.size)); err != nil {
		nf.Close()
		return err
	}
	nd, err := unix.Mmap(int(nf.Fd()), 0, int(s.size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		nf.Close()
		return err
	}
	binary.LittleEndian.PutUint32(nd[0:4], magic)
	binary.LittleEndian.PutUint32(nd[4:8], version)
	nd[16] = 0
	newIdx := make(map[string]loc, len(keys))
	w := uint64(headerSize)
	var live uint64
	for _, k := range keys {
		l := s.idx[k]
		val := s.data[l.valOff : l.valOff+uint64(l.valLen)]
		span := recordSpan(len(k), len(val))
		p := w
		nd[p] = recLive
		p++
		n := binary.PutUvarint(nd[p:], uint64(len(k)))
		p += uint64(n)
		copy(nd[p:], k)
		p += uint64(len(k))
		n = binary.PutUvarint(nd[p:], uint64(len(val)))
		p += uint64(n)
		valOff := p
		copy(nd[p:], val)
		newIdx[k] = loc{recOff: w, valOff: valOff, valLen: l.valLen, span: span}
		w += uint64(span)
		live += uint64(span)
	}
	binary.LittleEndian.PutUint64(nd[8:16], w)
	if err := unix.Msync(nd, unix.MS_SYNC); err != nil {
		unix.Munmap(nd)
		nf.Close()
		return err
	}
	// Swap: unmap old, rename new over it, adopt new mapping.
	unix.Munmap(s.data)
	s.f.Close()
	if err := os.Rename(tmpPath, s.dataPath); err != nil {
		unix.Munmap(nd)
		nf.Close()
		return err
	}
	s.f = nf
	s.data = nd
	s.idx = newIdx
	s.writeOff = w
	s.liveBytes = live
	s.dirty = false
	return nil
}

// Close flushes (clean shutdown) and unmaps.
func (s *Store) Close() error {
	var ferr error
	if s.data != nil {
		ferr = s.Flush()
		_ = unix.Munmap(s.data)
		s.data = nil
	}
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	return ferr
}
