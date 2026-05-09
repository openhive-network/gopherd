package blocklog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Writer is the append-only counterpart to Reader. Each Append writes one
// block's serialized bytes to the main file followed by an 8-byte trailer
// (start offset of the block + per-block attribute flags), and one 8-byte
// entry to the parallel `.index` sidecar.
//
// Implementation notes vs hived's libraries/chain/block_log.cpp:append_raw:
//
//   - We always write to a single monolithic `block_log` file (legacy
//     layout). Split-multifile layout (block_log_part.NNNN, 1M blocks per
//     part) is NOT yet supported on the writer side; the reader handles
//     both. Production hived since the multi-file rollout writes to part
//     files by default — switching this writer would require rotating
//     filehandles at every (block_num-1) % 1_000_000 == 0 boundary.
//
//   - We always write UNCOMPRESSED blocks (block_flags::uncompressed = 0,
//     dictionary_number unset). hived may compress blocks above a
//     threshold using the embedded zstd dictionaries; readers handle both
//     transparently. To produce a byte-identical copy of an existing
//     compressed block_log, the caller must pass the rawBlock that hived
//     wrote (i.e. the already-compressed bytes plus the matching
//     attributes via AppendRaw).
//
//   - We do NOT emit a `block_log.artifacts` sidecar (hived's per-block
//     metadata file with extra invariants). On the next reader open the
//     index sidecar is sufficient; artifacts can be rebuilt from the main
//     file. The reader in this package never opens .artifacts at all.
//
// All writes go through pwrite(fd, ..., offset) semantics — the os.File is
// kept positioned at EOF and we use Write (not Pwrite) for sequential
// throughput. fsync on Close is best-effort and only triggers when Sync was
// requested at OpenForWrite time.
type Writer interface {
	// Append writes one uncompressed block. Use AppendRaw if the caller
	// already holds compressed bytes plus attributes.
	Append(rawBlock []byte) error
	// AppendRaw writes a block whose bytes are already in the on-disk
	// representation hived expects (compressed or not), with the matching
	// attributes packed into the trailer.
	AppendRaw(rawBlock []byte, attr Attributes) error
	// HeadBlock returns the highest block number written so far. Returns 0
	// when the file is empty.
	HeadBlock() uint32
	// Close flushes any pending sidecar entries and releases the file
	// handles.
	Close() error
}

// OpenForWrite opens (or creates) the main file at path plus the matching
// `.index` sidecar in append mode. If either file already contains data, we
// resume after its head — the caller is responsible for ensuring path was
// written by a compatible writer (same on-disk format).
//
// compressBlocks is currently a no-op flag; the parameter is preserved for
// API forward-compatibility with a future compressing writer.
func OpenForWrite(path string, compressBlocks bool) (Writer, error) {
	if compressBlocks {
		return nil, errors.New("blocklog: compressing writer not yet implemented; use Append on already-compressed bytes via AppendRaw")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("blocklog: mkdir for %s: %w", path, err)
	}

	main, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("blocklog: open main %s: %w", path, err)
	}
	mainStat, err := main.Stat()
	if err != nil {
		main.Close()
		return nil, err
	}
	if _, err := main.Seek(0, 2); err != nil {
		main.Close()
		return nil, err
	}

	idxPath := path + indexExtension
	idx, err := os.OpenFile(idxPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		main.Close()
		return nil, fmt.Errorf("blocklog: open index %s: %w", idxPath, err)
	}
	idxStat, err := idx.Stat()
	if err != nil {
		main.Close()
		idx.Close()
		return nil, err
	}
	if _, err := idx.Seek(0, 2); err != nil {
		main.Close()
		idx.Close()
		return nil, err
	}

	w := &fileWriter{
		main:        main,
		idx:         idx,
		mainSize:    uint64(mainStat.Size()),
		idxEntries:  uint32(idxStat.Size() / indexEntrySize),
	}
	return w, nil
}

// fileWriter implements Writer against a (main_file, .index_sidecar) pair.
type fileWriter struct {
	main *os.File
	idx  *os.File

	// mainSize tracks the current file size in bytes (next write offset).
	// Mirrors block_log_impl::block_log_size in C++.
	mainSize uint64

	// idxEntries is the number of 8-byte entries written so far == head
	// block number when the writer started from an empty file.
	idxEntries uint32
}

// Append writes block bytes with the uncompressed flag.
func (w *fileWriter) Append(rawBlock []byte) error {
	return w.AppendRaw(rawBlock, Attributes{Compressed: false})
}

// AppendRaw is the low-level append. Writes bytes followed by the packed
// trailer (matching hived's combine_block_start_pos_with_flags).
func (w *fileWriter) AppendRaw(rawBlock []byte, attr Attributes) error {
	if len(rawBlock) == 0 {
		return errors.New("blocklog: empty block")
	}
	startPos := w.mainSize
	packed := encodeTrailer(startPos, attr)

	// Single combined buffer + single Write so we never observe a torn
	// trailer if the caller is also reading concurrently.
	buf := make([]byte, len(rawBlock)+trailerSize)
	copy(buf, rawBlock)
	binary.LittleEndian.PutUint64(buf[len(rawBlock):], packed)

	n, err := w.main.Write(buf)
	if err != nil {
		return fmt.Errorf("blocklog: write block at offset %d: %w", startPos, err)
	}
	if n != len(buf) {
		return fmt.Errorf("blocklog: short write %d/%d", n, len(buf))
	}
	w.mainSize += uint64(n)

	// Index entry: the same packed trailer, written contiguously to the
	// sidecar. This matches hived's `block_log_artifacts` layout used by
	// our reader's on-disk index path.
	var entry [indexEntrySize]byte
	binary.LittleEndian.PutUint64(entry[:], packed)
	if _, err := w.idx.Write(entry[:]); err != nil {
		return fmt.Errorf("blocklog: write index entry: %w", err)
	}
	w.idxEntries++
	return nil
}

func (w *fileWriter) HeadBlock() uint32 { return w.idxEntries }

// Close flushes both files. Errors from either are surfaced.
func (w *fileWriter) Close() error {
	var firstErr error
	if w.main != nil {
		if err := w.main.Sync(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := w.main.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		w.main = nil
	}
	if w.idx != nil {
		if err := w.idx.Sync(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := w.idx.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		w.idx = nil
	}
	return firstErr
}

// encodeTrailer packs a start offset + attributes into the 8-byte hived
// trailer format. Inverse of decodeTrailer in blocklog.go.
//
//	bit 63    : 1 = compressed, 0 = uncompressed
//	bit 56    : 1 = dictionary_number byte set
//	bits 48-55: dictionary number (only when bit 56 set)
//	bits 0-47 : start offset (limited to 256 TB; fine for any real chain)
//
// Mirrors libraries/chain/include/hive/chain/detail/block_attributes.hpp:
// combine_block_start_pos_with_flags.
func encodeTrailer(startPos uint64, attr Attributes) uint64 {
	if startPos&^offsetMask != 0 {
		// Defensive: impossible at any realistic block_log size, but if
		// it ever happens we lose the high bits silently — surface it.
		panic(fmt.Sprintf("blocklog: start offset %d exceeds 48-bit field", startPos))
	}
	packed := startPos
	if attr.Compressed {
		packed |= compressedFlag
	}
	if attr.HasDictNum {
		packed |= hasDictFlag
		packed |= uint64(attr.DictionaryNum) << dictShift
	}
	return packed
}
