// Package blocklog reads hived-compatible block_log files (legacy single-file
// and split-multifile layouts).
//
// On-disk layout (libraries/chain/block_log.cpp):
//
//	block_log{,_part.NNNN}                main append-only file
//	  +---------+----------+---------+----------+-----+----------+----------+
//	  | block 1 | trailer1 | block 2 | trailer2 | ... | block H  | trailerH |
//	  +---------+----------+---------+----------+-----+----------+----------+
//	  Each "trailer" is 8 bytes encoding the start offset of the *preceding*
//	  block plus per-block attribute flags. Format (little-endian uint64):
//	     bit  63    : 1 = zstd-compressed, 0 = uncompressed
//	     bit  56    : 1 = `dictionary_number` slot is meaningful
//	     bits 48–55 : dictionary_number (only if bit 56 is set)
//	     bits  0–47 : start offset of the preceding block
//	  See libraries/chain/include/hive/chain/detail/block_attributes.hpp.
//
//	block_log{,_part.NNNN}.index          parallel sidecar file
//	  +-----------+-----------+-----+-----------+
//	  | trailer1  | trailer2  | ... | trailerH  |
//	  +-----------+-----------+-----+-----------+
//	  Same 8-byte format as the main-file trailers, just packed contiguously
//	  for O(1) lookup. When this sidecar is missing, Open() builds the
//	  equivalent in memory by walking the main file backward via the trailer
//	  chain — slower but functionally identical.
//
// Compressed blocks are transparently decompressed on Block(N) via the
// embedded dictionary set in internal/blocklog/zstd.
package blocklog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"hived-go/internal/blocklog/zstd"
)

const (
	trailerSize       = 8
	indexEntrySize    = 8
	defaultPartBlocks = 1_000_000
	legacyFileName    = "block_log"
	splitFileNameCore = "block_log_part"
	indexExtension    = ".index"

	// offsetMask isolates the 48-bit start offset within a packed trailer.
	offsetMask uint64 = 0x0000_ffff_ffff_ffff
	// compressedFlag is bit 63 of a trailer: 1 = zstd-compressed.
	compressedFlag uint64 = 1 << 63
	// hasDictFlag is bit 56 of a trailer: 1 = dictionary_number byte set.
	hasDictFlag uint64 = 1 << 56
	// dictShift extracts the dictionary_number byte (bits 48–55).
	dictShift = 48
	dictMask  = 0xff
)

// Attributes captures the per-block metadata packed into trailer high bits.
type Attributes struct {
	Compressed     bool
	DictionaryNum  uint8 // valid only when HasDictNum is true
	HasDictNum     bool
}

func decodeTrailer(packed uint64) (offset uint64, attr Attributes) {
	offset = packed & offsetMask
	attr.Compressed = packed&compressedFlag != 0
	if packed&hasDictFlag != 0 {
		attr.HasDictNum = true
		attr.DictionaryNum = uint8((packed >> dictShift) & dictMask)
	}
	return
}

// Reader is a read-only view of one or more block_log files.
type Reader struct {
	parts        []*part
	headBlock    uint32
	decompressor *zstd.Decompressor
}

// part covers one block_log{,_part.NNNN} file plus its sidecar / in-memory index.
type part struct {
	mainPath string
	main     *os.File
	mainSize int64

	first uint32 // first block num in this part
	count uint32 // number of blocks in this part

	// indexFile is non-nil when an on-disk .index sidecar was found.
	indexFile *os.File

	// indexMem is non-nil when we rebuilt the index in memory because no
	// sidecar was present. Each entry is the packed trailer (offset+flags).
	indexMem []uint64
}

// Open opens a block_log given any path inside the data dir — either the
// legacy `block_log` filename or one of the split parts (`block_log_part.NNNN`).
// The reader auto-discovers the rest of the parts and the .index sidecars,
// rebuilding any missing index in memory by walking the main file backward.
func Open(path string) (*Reader, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	parts, err := discoverParts(dir, base)
	if err != nil {
		return nil, err
	}

	r := &Reader{
		parts:        parts,
		decompressor: zstd.NewDecompressor(),
	}

	for _, p := range parts {
		main, err := os.Open(p.mainPath)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("blocklog: open main %s: %w", p.mainPath, err)
		}
		stat, err := main.Stat()
		if err != nil {
			main.Close()
			r.Close()
			return nil, err
		}
		p.main = main
		p.mainSize = stat.Size()

		idxPath := p.mainPath + indexExtension
		if idxFile, err := os.Open(idxPath); err == nil {
			idxStat, statErr := idxFile.Stat()
			if statErr != nil {
				idxFile.Close()
				r.Close()
				return nil, statErr
			}
			p.indexFile = idxFile
			p.count = uint32(idxStat.Size() / indexEntrySize)
		} else if errors.Is(err, fs.ErrNotExist) {
			if err := buildInMemoryIndex(p); err != nil {
				r.Close()
				return nil, fmt.Errorf("blocklog: rebuild index for %s: %w", p.mainPath, err)
			}
		} else {
			r.Close()
			return nil, err
		}
	}

	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i].count > 0 {
			r.headBlock = parts[i].first + parts[i].count - 1
			break
		}
	}
	return r, nil
}

// Close releases all underlying file handles and decoder pools.
func (r *Reader) Close() error {
	var firstErr error
	for _, p := range r.parts {
		if p.main != nil {
			if err := p.main.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
			p.main = nil
		}
		if p.indexFile != nil {
			if err := p.indexFile.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
			p.indexFile = nil
		}
	}
	if r.decompressor != nil {
		r.decompressor.Close()
		r.decompressor = nil
	}
	return firstErr
}

// Head returns the last block number stored in the log.
func (r *Reader) Head() uint32 { return r.headBlock }

// Block reads the raw FC-binary bytes for blockNum. Compressed blocks are
// transparently decompressed. Returned slice is owned by the caller.
func (r *Reader) Block(blockNum uint32) ([]byte, error) {
	if blockNum == 0 || blockNum > r.headBlock {
		return nil, fmt.Errorf("blocklog: block %d out of range [1, %d]", blockNum, r.headBlock)
	}
	p := r.partFor(blockNum)
	if p == nil {
		return nil, fmt.Errorf("blocklog: no part covers block %d", blockNum)
	}

	relIdx := blockNum - p.first
	packed, err := p.lookupTrailer(relIdx)
	if err != nil {
		return nil, fmt.Errorf("blocklog: lookup block %d: %w", blockNum, err)
	}
	off, attr := decodeTrailer(packed)

	endOff, err := p.endOffset(relIdx)
	if err != nil {
		return nil, fmt.Errorf("blocklog: end offset for block %d: %w", blockNum, err)
	}
	if endOff < off {
		return nil, fmt.Errorf("blocklog: corrupt offsets for block %d (start %d > end %d)", blockNum, off, endOff)
	}

	raw := make([]byte, endOff-off)
	if _, err := p.main.ReadAt(raw, int64(off)); err != nil {
		return nil, fmt.Errorf("blocklog: read block %d: %w", blockNum, err)
	}

	if !attr.Compressed {
		return raw, nil
	}
	if !attr.HasDictNum {
		// Compressed without an explicit dictionary number — uncommon in mainnet
		// but supported by the format. Try the no-dict decoder.
		out, err := r.decompressor.DecompressNoDict(raw, nil)
		if err != nil {
			return nil, fmt.Errorf("blocklog: decompress block %d (no dict): %w", blockNum, err)
		}
		return out, nil
	}
	out, err := r.decompressor.Decompress(raw, attr.DictionaryNum, nil)
	if err != nil {
		return nil, fmt.Errorf("blocklog: decompress block %d (dict %d): %w", blockNum, attr.DictionaryNum, err)
	}
	return out, nil
}

// RawBlockBytes returns the on-disk bytes for blockNum WITHOUT decompressing
// them, plus the block's attributes (so callers know whether to decompress
// themselves). Used by the parity tests in cmd/ghive-sync to compare a freshly
// re-compressed slice against the source file.
func (r *Reader) RawBlockBytes(blockNum uint32) ([]byte, Attributes, error) {
	if blockNum == 0 || blockNum > r.headBlock {
		return nil, Attributes{}, fmt.Errorf("blocklog: block %d out of range [1, %d]", blockNum, r.headBlock)
	}
	p := r.partFor(blockNum)
	if p == nil {
		return nil, Attributes{}, fmt.Errorf("blocklog: no part covers block %d", blockNum)
	}
	relIdx := blockNum - p.first
	packed, err := p.lookupTrailer(relIdx)
	if err != nil {
		return nil, Attributes{}, fmt.Errorf("blocklog: lookup block %d: %w", blockNum, err)
	}
	off, attr := decodeTrailer(packed)
	endOff, err := p.endOffset(relIdx)
	if err != nil {
		return nil, Attributes{}, fmt.Errorf("blocklog: end offset for block %d: %w", blockNum, err)
	}
	if endOff < off {
		return nil, Attributes{}, fmt.Errorf("blocklog: corrupt offsets for block %d (start %d > end %d)", blockNum, off, endOff)
	}
	raw := make([]byte, endOff-off)
	if _, err := p.main.ReadAt(raw, int64(off)); err != nil {
		return nil, Attributes{}, fmt.Errorf("blocklog: read block %d: %w", blockNum, err)
	}
	return raw, attr, nil
}

// Attributes returns the parsed attributes for blockNum without touching the
// main file. Useful for diagnostics.
func (r *Reader) Attributes(blockNum uint32) (Attributes, error) {
	if blockNum == 0 || blockNum > r.headBlock {
		return Attributes{}, fmt.Errorf("blocklog: block %d out of range", blockNum)
	}
	p := r.partFor(blockNum)
	if p == nil {
		return Attributes{}, fmt.Errorf("blocklog: no part covers block %d", blockNum)
	}
	packed, err := p.lookupTrailer(blockNum - p.first)
	if err != nil {
		return Attributes{}, err
	}
	_, attr := decodeTrailer(packed)
	return attr, nil
}

// partFor returns the part covering blockNum, or nil if none.
func (r *Reader) partFor(blockNum uint32) *part {
	for _, p := range r.parts {
		if p.count == 0 {
			continue
		}
		if blockNum >= p.first && blockNum < p.first+p.count {
			return p
		}
	}
	return nil
}

// lookupTrailer returns the packed 8-byte trailer for the blockIdx-th block
// in this part (0-indexed within the part).
func (p *part) lookupTrailer(blockIdx uint32) (uint64, error) {
	if p.indexFile != nil {
		var buf [indexEntrySize]byte
		if _, err := p.indexFile.ReadAt(buf[:], int64(blockIdx)*indexEntrySize); err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint64(buf[:]), nil
	}
	if int(blockIdx) >= len(p.indexMem) {
		return 0, fmt.Errorf("block index %d past end (count=%d)", blockIdx, len(p.indexMem))
	}
	return p.indexMem[blockIdx], nil
}

// endOffset returns the byte offset just past the blockIdx-th block in this
// part (i.e. the start of that block's trailer).
func (p *part) endOffset(blockIdx uint32) (uint64, error) {
	if blockIdx+1 < p.count {
		next, err := p.lookupTrailer(blockIdx + 1)
		if err != nil {
			return 0, err
		}
		nextOff, _ := decodeTrailer(next)
		return nextOff - trailerSize, nil
	}
	return uint64(p.mainSize) - trailerSize, nil
}

// buildInMemoryIndex walks the main file backward from EOF, following the
// trailer chain to enumerate every block's start offset and attributes.
// O(N) syscalls amortized via 1 MB rolling-window reads.
func buildInMemoryIndex(p *part) error {
	const chunkSize = 1 << 20

	if p.mainSize < trailerSize {
		return fmt.Errorf("main file too small: %d bytes", p.mainSize)
	}

	// Start by reading the very last 8 bytes — that's the head block's trailer.
	trailerPos := p.mainSize - trailerSize
	if trailerPos < 0 {
		return errors.New("main file shorter than 8 bytes")
	}

	chunk := make([]byte, 0, chunkSize)
	chunkStart := int64(-1)
	chunkEnd := int64(-1) // exclusive

	// trailers will hold packed trailer values in *reverse* discovery order
	// (head first); we reverse at the end.
	trailers := make([]uint64, 0, 1<<20)

	for trailerPos >= 0 {
		// Ensure we have [trailerPos, trailerPos+8) inside the chunk window.
		if trailerPos < chunkStart || trailerPos+trailerSize > chunkEnd {
			// Refill chunk: window ending at trailerPos+8.
			windowEnd := trailerPos + trailerSize
			windowStart := windowEnd - chunkSize
			if windowStart < 0 {
				windowStart = 0
			}
			chunk = chunk[:windowEnd-windowStart]
			if _, err := p.main.ReadAt(chunk, windowStart); err != nil {
				return fmt.Errorf("read chunk at %d..%d: %w", windowStart, windowEnd, err)
			}
			chunkStart = windowStart
			chunkEnd = windowEnd
		}
		off := trailerPos - chunkStart
		packed := binary.LittleEndian.Uint64(chunk[off : off+trailerSize])
		trailers = append(trailers, packed)

		blockOffset, _ := decodeTrailer(packed)
		// Block 1 has blockOffset == 0, and we should stop after recording it.
		if blockOffset == 0 {
			break
		}
		// Next block's trailer ends at blockOffset (since [blockOffset, ...) is
		// the current block, and the previous trailer sits at blockOffset-8).
		trailerPos = int64(blockOffset) - trailerSize
	}

	// Reverse so trailers[0] is block 1 (relative to this part).
	for i, j := 0, len(trailers)-1; i < j; i, j = i+1, j-1 {
		trailers[i], trailers[j] = trailers[j], trailers[i]
	}

	p.indexMem = trailers
	p.count = uint32(len(trailers))
	return nil
}

// --- File-discovery helpers ----------------------------------------------

func discoverParts(dir, base string) ([]*part, error) {
	if base == legacyFileName {
		mainPath := filepath.Join(dir, legacyFileName)
		if _, err := os.Stat(mainPath); err != nil {
			return nil, fmt.Errorf("blocklog: missing %s: %w", mainPath, err)
		}
		return []*part{{mainPath: mainPath, first: 1}}, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type discovered struct {
		num  uint32
		name string
	}
	var found []discovered
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, splitFileNameCore+".") || strings.HasSuffix(name, indexExtension) {
			continue
		}
		ext := strings.TrimPrefix(name, splitFileNameCore+".")
		if len(ext) != 4 {
			continue
		}
		var n uint32
		valid := true
		for _, c := range ext {
			if c < '0' || c > '9' {
				valid = false
				break
			}
			n = n*10 + uint32(c-'0')
		}
		if !valid || n == 0 {
			continue
		}
		found = append(found, discovered{num: n, name: name})
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("blocklog: no part files matching %s.NNNN under %s", splitFileNameCore, dir)
	}

	sort.Slice(found, func(i, j int) bool { return found[i].num < found[j].num })

	first := found[0].num
	for i, f := range found {
		if f.num != first+uint32(i) {
			return nil, fmt.Errorf("blocklog: non-contiguous part numbering near %s", f.name)
		}
	}

	parts := make([]*part, len(found))
	for i, f := range found {
		mainPath := filepath.Join(dir, f.name)
		startBlock := uint32(1) + (f.num-1)*defaultPartBlocks
		parts[i] = &part{mainPath: mainPath, first: startBlock}
	}
	return parts, nil
}

// fs.ErrNotExist alias to keep the import surface lean.
var fs = struct {
	ErrNotExist error
}{ErrNotExist: os.ErrNotExist}

var _ = io.SeekStart // silence unused import if io ever gets dropped
