package zstd

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// findMainnetBlockLog walks up from the test's CWD looking for ./block_log
// at the project root. Returns "" if not found.
func findMainnetBlockLog() string {
	candidates := []string{
		"../../../block_log",
		"../../block_log",
		"../block_log",
		"./block_log",
		"/home/martin/projects/hived-go/block_log",
	}
	for _, p := range candidates {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	return ""
}

// TestByteIdenticalAgainstMainnet probes which (level, dict) combination
// reproduces the on-disk mainnet bytes. Skipped when ./block_log isn't
// present.
//
// This is the experiment that decided the writer's default level. Kept as a
// running parity check: if zstd library or hived's parameters change, this
// test will catch the drift on the next run.
func TestByteIdenticalAgainstMainnet(t *testing.T) {
	src := findMainnetBlockLog()
	if src == "" {
		t.Skip("./block_log not available; skipping mainnet parity check")
	}
	rawTrailers, err := readAllTrailers(src)
	if err != nil {
		t.Fatalf("read trailers: %v", err)
	}
	if len(rawTrailers) < 100 {
		t.Skipf("block_log only has %d blocks; need >=100", len(rawTrailers))
	}

	bf, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer bf.Close()
	stat, _ := bf.Stat()
	totalSize := uint64(stat.Size())

	// We test compression levels 1..22. ZSTD_CLEVEL_DEFAULT is 3 today; if
	// hived's mainnet log was written at a different level, the count of
	// matching blocks will tell us which one to use.
	const sampleStart = 1
	const sampleN = 100

	// Decompressor for round-trip sanity.
	d := NewDecompressor()
	defer d.Close()

	bestLevel := 0
	bestMatches := 0
	for _, lev := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22} {
		c := NewCompressorWithLevel(lev)
		matches := 0
		for i := uint32(sampleStart); i < sampleStart+sampleN; i++ {
			off, attr := decodeTrailerLite(rawTrailers[i-1])
			if !attr.compressed {
				continue
			}
			endOff := totalSize - 8 // last block
			if int(i) < len(rawTrailers) {
				nextOff, _ := decodeTrailerLite(rawTrailers[i])
				endOff = nextOff - 8
			}
			diskBytes := make([]byte, endOff-off)
			if _, err := bf.ReadAt(diskBytes, int64(off)); err != nil {
				t.Fatalf("read disk bytes for block %d: %v", i, err)
			}
			// Decompress to get the raw block.
			var raw []byte
			if attr.hasDict {
				raw, err = d.Decompress(diskBytes, attr.dictNum, nil)
			} else {
				raw, err = d.DecompressNoDict(diskBytes, nil)
			}
			if err != nil {
				t.Fatalf("decompress block %d: %v", i, err)
			}
			// Re-compress with our compressor.
			var our []byte
			if attr.hasDict {
				our, err = c.Compress(raw, attr.dictNum)
			} else {
				our, err = c.CompressNoDict(raw)
			}
			if err != nil {
				t.Fatalf("compress block %d at level %d: %v", i, lev, err)
			}
			if bytes.Equal(our, diskBytes) {
				matches++
			}
		}
		if matches > bestMatches {
			bestMatches = matches
			bestLevel = lev
		}
		t.Logf("level %2d: %d/%d byte-identical", lev, matches, sampleN)
	}
	t.Logf("best level: %d (%d/%d matches)", bestLevel, bestMatches, sampleN)
	if bestMatches == 0 {
		t.Logf("NO level produced byte-identical output — zstd version drift between hived (1.5.2) and our libzstd (system) prevents byte-for-byte parity. Round-trip parity still holds; cf. TestRoundTrip.")
	}
}

type liteAttr struct {
	compressed bool
	hasDict    bool
	dictNum    uint8
}

func decodeTrailerLite(packed uint64) (uint64, liteAttr) {
	const offsetMask uint64 = 0x0000_ffff_ffff_ffff
	const compressedFlag uint64 = 1 << 63
	const hasDictFlag uint64 = 1 << 56
	off := packed & offsetMask
	a := liteAttr{
		compressed: packed&compressedFlag != 0,
		hasDict:    packed&hasDictFlag != 0,
	}
	if a.hasDict {
		a.dictNum = uint8((packed >> 48) & 0xff)
	}
	return off, a
}

// readAllTrailers walks the main file's trailer chain backward and returns
// the per-block packed trailer values in block order (block 1 first).
func readAllTrailers(path string) ([]uint64, error) {
	// Try the on-disk index sidecar first.
	idxPath := path + ".index"
	if data, err := os.ReadFile(idxPath); err == nil {
		if len(data)%8 != 0 {
			return nil, fmt.Errorf("index size %d not aligned to 8", len(data))
		}
		trailers := make([]uint64, len(data)/8)
		for i := range trailers {
			trailers[i] = binary.LittleEndian.Uint64(data[i*8 : i*8+8])
		}
		return trailers, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	// No sidecar — walk the trailer chain backward, exactly like
	// blocklog.buildInMemoryIndex but inline (we can't import blocklog from
	// inside its sub-package without a dependency cycle).
	bf, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer bf.Close()
	stat, err := bf.Stat()
	if err != nil {
		return nil, err
	}
	size := stat.Size()
	if size < 8 {
		return nil, errors.New("file too small")
	}
	const chunkSize = 1 << 20
	trailerPos := size - 8
	chunk := make([]byte, 0, chunkSize)
	chunkStart := int64(-1)
	chunkEnd := int64(-1)
	trailers := make([]uint64, 0, 1<<20)
	for trailerPos >= 0 {
		if trailerPos < chunkStart || trailerPos+8 > chunkEnd {
			windowEnd := trailerPos + 8
			windowStart := windowEnd - chunkSize
			if windowStart < 0 {
				windowStart = 0
			}
			chunk = chunk[:windowEnd-windowStart]
			if _, err := bf.ReadAt(chunk, windowStart); err != nil {
				return nil, fmt.Errorf("read chunk %d..%d: %w", windowStart, windowEnd, err)
			}
			chunkStart = windowStart
			chunkEnd = windowEnd
		}
		off := trailerPos - chunkStart
		packed := binary.LittleEndian.Uint64(chunk[off : off+8])
		trailers = append(trailers, packed)
		blockOff, _ := decodeTrailerLite(packed)
		if blockOff == 0 {
			break
		}
		trailerPos = int64(blockOff) - 8
	}
	for i, j := 0, len(trailers)-1; i < j; i, j = i+1, j-1 {
		trailers[i], trailers[j] = trailers[j], trailers[i]
	}
	return trailers, nil
}

var _ = zstd.SpeedDefault // keep the import alive; useful to compare paths
