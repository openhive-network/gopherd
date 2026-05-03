package state

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// snapshotMagic identifies a ghive state snapshot stream. Bumped only on
// breaking format changes (encoding-incompatible — readers reject older
// magic instead of trying to interpret).
var snapshotMagic = [8]byte{'G', 'H', 'I', 'V', 'E', 'S', 'N', 'P'}

// snapshotVersion is the on-disk format version. v1: stream of
// [keylen-uvarint][key][vallen-uvarint][value] records terminated by
// keylen==0. Header is magic + version + headBlockNum.
const snapshotVersion uint32 = 1

// SaveSnapshot streams every (physical-key, value) pair in d to path,
// preceded by a small header carrying the chain head block number.
//
// The snapshot captures the raw KV layer; per-Database transient caches
// (expHints, Aux) are NOT serialized since they are reconstructable from
// the rebuilt state on first access.
func (d *Database) SaveSnapshot(path string, headBlockNum uint32) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("snapshot: create %s: %w", path, err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)

	// Header.
	if _, err := w.Write(snapshotMagic[:]); err != nil {
		return err
	}
	var hdr [16]byte
	binary.LittleEndian.PutUint32(hdr[0:4], snapshotVersion)
	binary.LittleEndian.PutUint32(hdr[4:8], headBlockNum)
	if _, err := w.Write(hdr[:8]); err != nil {
		return err
	}

	// Iterate every key. Use [{}, 0xff*8] as bounds — all keys begin
	// with [tableID:2][indexID:1] so 0xff repeated past the maximum
	// tableID safely upper-bounds every real key.
	upper := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	it, err := d.backend.rawIter([]byte{}, upper)
	if err != nil {
		return err
	}
	defer it.Close()

	var lenBuf [binary.MaxVarintLen64]byte
	count := 0
	for ok := it.First(); ok; ok = it.Next() {
		key := it.Key()
		val, verr := it.ValueAndErr()
		if verr != nil {
			return fmt.Errorf("snapshot: iter value: %w", verr)
		}
		n := binary.PutUvarint(lenBuf[:], uint64(len(key)))
		if _, err := w.Write(lenBuf[:n]); err != nil {
			return err
		}
		if _, err := w.Write(key); err != nil {
			return err
		}
		n = binary.PutUvarint(lenBuf[:], uint64(len(val)))
		if _, err := w.Write(lenBuf[:n]); err != nil {
			return err
		}
		if _, err := w.Write(val); err != nil {
			return err
		}
		count++
	}
	if err := it.Error(); err != nil {
		return fmt.Errorf("snapshot: iter: %w", err)
	}
	// Sentinel: keylen=0.
	if _, err := w.Write([]byte{0x00}); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Sync()
}

// LoadSnapshot reads a snapshot written by SaveSnapshot and re-inserts
// every (key, value) into d, replacing whatever the backend currently
// holds for those keys. Returns the head block number recorded in the
// snapshot header.
//
// Designed to be called against a freshly-opened (empty) Database.
// Calling it on a Database with pre-existing rows is undefined — collisions
// are overwritten but pre-existing keys not in the snapshot are left
// untouched.
func (d *Database) LoadSnapshot(path string) (uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("snapshot: open %s: %w", path, err)
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)

	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return 0, fmt.Errorf("snapshot: read magic: %w", err)
	}
	if magic != snapshotMagic {
		return 0, fmt.Errorf("snapshot: bad magic %q", magic)
	}
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, fmt.Errorf("snapshot: read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[0:4])
	if ver != snapshotVersion {
		return 0, fmt.Errorf("snapshot: unsupported version %d", ver)
	}
	headBlockNum := binary.LittleEndian.Uint32(hdr[4:8])

	// Stream records into a single overlay batch and commit at the end.
	// Sized generously — mainnet at block 8M has ~few-million keys.
	overlay := make(map[string]overlayEntry, 1<<20)
	for {
		klen, err := binary.ReadUvarint(r)
		if err != nil {
			return 0, fmt.Errorf("snapshot: read key len: %w", err)
		}
		if klen == 0 {
			break
		}
		key := make([]byte, klen)
		if _, err := io.ReadFull(r, key); err != nil {
			return 0, fmt.Errorf("snapshot: read key: %w", err)
		}
		vlen, err := binary.ReadUvarint(r)
		if err != nil {
			return 0, fmt.Errorf("snapshot: read val len: %w", err)
		}
		val := make([]byte, vlen)
		if _, err := io.ReadFull(r, val); err != nil {
			return 0, fmt.Errorf("snapshot: read val: %w", err)
		}
		overlay[string(key)] = overlayEntry{value: val}
	}
	if err := d.backend.commit(overlay, true); err != nil {
		return 0, fmt.Errorf("snapshot: commit: %w", err)
	}
	return headBlockNum, nil
}
