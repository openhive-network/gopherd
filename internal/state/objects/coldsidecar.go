package objects

// Cold-tier bookkeeping sidecar.
//
// WHY. With GHIVE_COLD_REUSE=1 the load already skips rewriting the cold rows
// (that alone took startup from 23.9 min to 4.3 min). What remains is that the
// loader STILL streams all 26 GB of comment_store purely to rebuild two things,
// per spillToColdReuse:
//
//	s.bloom.add(key)   // the cold-membership filter, ~128 MB of uint64s
//	s.coldTotal++      // a single counter
//	s.coldRoot[...]++  // only when coldByRootEnabled, which is false post-HF17
//
// So 218 seconds and 26 GB of I/O produce a bloom filter and an integer. Both
// are pure functions of the cold tier's contents, and the cold tier is already
// on disk and unchanged. Writing them beside it removes the read entirely.
//
// This is the same shape as the rest of the startup work: stop recomputing what
// can be stored. hived doesn't rebuild anything at boot, it mmaps
// shared_memory.bin and is ready.
//
// SAFETY. The sidecar is only valid for the exact (cold tier, snapshot) pair it
// was written from, which is the same contract GHIVE_COLD_REUSE already carries.
// The header records the snapshot block and the cold-row count; load refuses on
// any mismatch and falls back to the full scan. A stale bloom would be
// especially nasty (false negatives silently hide cold rows, so a comment reads
// as missing), so this errs toward rejecting.

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

const (
	coldSidecarMagic   = "GHCS"
	coldSidecarVersion = uint32(1)
	coldSidecarName    = "cold_bookkeeping.bin"
)

// ColdSidecarPath is where the sidecar lives for a given cold dir.
func ColdSidecarPath(coldDir string) string { return filepath.Join(coldDir, coldSidecarName) }

// SaveColdSidecar writes the bloom + counters for `block` next to the cold tier.
// Called after a load (or before shutdown) so the NEXT start can skip the scan.
func (s *CommentStore) SaveColdSidecar(coldDir string, block uint32) error {
	if s.bloom == nil {
		return fmt.Errorf("cold sidecar: no bloom to save")
	}
	tmp := ColdSidecarPath(coldDir) + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	hdr := make([]byte, 0, 64)
	hdr = append(hdr, coldSidecarMagic...)
	hdr = binary.LittleEndian.AppendUint32(hdr, coldSidecarVersion)
	hdr = binary.LittleEndian.AppendUint32(hdr, block)
	hdr = binary.LittleEndian.AppendUint64(hdr, uint64(s.coldTotal))
	hdr = binary.LittleEndian.AppendUint64(hdr, s.bloom.mask)
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(s.bloom.k))
	hdr = binary.LittleEndian.AppendUint64(hdr, uint64(len(s.bloom.bits)))
	if _, err := f.Write(hdr); err != nil {
		f.Close()
		return err
	}
	// Bits as raw little-endian uint64s, with a CRC so a torn write is caught
	// rather than silently loaded as a corrupt filter.
	crc := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	buf := make([]byte, 1<<16)
	for i := 0; i < len(s.bloom.bits); {
		n := 0
		for ; n+8 <= len(buf) && i < len(s.bloom.bits); n, i = n+8, i+1 {
			binary.LittleEndian.PutUint64(buf[n:], s.bloom.bits[i])
		}
		if _, err := f.Write(buf[:n]); err != nil {
			f.Close()
			return err
		}
		crc.Write(buf[:n])
	}
	var tail [4]byte
	binary.LittleEndian.PutUint32(tail[:], crc.Sum32())
	if _, err := f.Write(tail[:]); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, ColdSidecarPath(coldDir))
}

// LoadColdSidecar restores the bloom + counters for `block`. Returns false (no
// error) when there is nothing usable, so the caller falls back to the scan.
//
// Every mismatch is a refusal: a bloom that does not correspond to this exact
// cold tier produces false NEGATIVES, which read as "comment does not exist"
// rather than as an error.
func (s *CommentStore) LoadColdSidecar(coldDir string, block uint32) (bool, error) {
	f, err := os.Open(ColdSidecarPath(coldDir))
	if err != nil {
		return false, nil // absent is the normal first-run case
	}
	defer f.Close()

	hdr := make([]byte, 4+4+4+8+8+4+8)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return false, nil
	}
	if string(hdr[0:4]) != coldSidecarMagic {
		return false, nil
	}
	off := 4
	ver := binary.LittleEndian.Uint32(hdr[off:])
	off += 4
	gotBlock := binary.LittleEndian.Uint32(hdr[off:])
	off += 4
	total := binary.LittleEndian.Uint64(hdr[off:])
	off += 8
	mask := binary.LittleEndian.Uint64(hdr[off:])
	off += 8
	k := binary.LittleEndian.Uint32(hdr[off:])
	off += 4
	nwords := binary.LittleEndian.Uint64(hdr[off:])

	if ver != coldSidecarVersion {
		return false, nil
	}
	if gotBlock != block {
		// Wrong vintage: the tier has moved on (or back) since this was written.
		return false, fmt.Errorf("cold sidecar: block mismatch (sidecar %d, snapshot %d) — ignoring", gotBlock, block)
	}
	if nwords == 0 || nwords > (1<<34) {
		return false, fmt.Errorf("cold sidecar: implausible bloom size %d — ignoring", nwords)
	}

	bits := make([]uint64, nwords)
	crc := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	buf := make([]byte, 1<<16)
	for i := uint64(0); i < nwords; {
		want := len(buf)
		if rem := int((nwords - i) * 8); rem < want {
			want = rem
		}
		if _, err := io.ReadFull(f, buf[:want]); err != nil {
			return false, fmt.Errorf("cold sidecar: truncated bloom: %w", err)
		}
		crc.Write(buf[:want])
		for n := 0; n+8 <= want; n += 8 {
			bits[i] = binary.LittleEndian.Uint64(buf[n:])
			i++
		}
	}
	var tail [4]byte
	if _, err := io.ReadFull(f, tail[:]); err != nil {
		return false, fmt.Errorf("cold sidecar: missing crc: %w", err)
	}
	if binary.LittleEndian.Uint32(tail[:]) != crc.Sum32() {
		return false, fmt.Errorf("cold sidecar: crc mismatch — ignoring")
	}

	s.bloom = &coldBloom{bits: bits, mask: mask, k: int(k)}
	s.coldTotal = int(total)
	return true, nil
}

// SetSidecarLoaded tells the load path that bloom + coldTotal are already
// restored, so the per-record rebuild in spillToColdReuse can be skipped.
func (s *CommentStore) SetSidecarLoaded(v bool) { s.sidecarLoaded = v }

// ColdTotal exposes the cold row count (for sidecar validation by the caller).
func (s *CommentStore) ColdTotal() int { return s.coldTotal }
