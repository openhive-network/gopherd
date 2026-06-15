// encoder.go — block-level zstd compression matching hived's on-disk format.
//
// Hived compresses each block with libzstd's advanced API and these flags
// (libraries/chain/block_log_compression.cpp):
//
//	ZSTD_c_format        = ZSTD_f_zstd1_magicless   (strip 4-byte magic)
//	ZSTD_c_contentSizeFlag = 0                      (no decompressed size)
//	ZSTD_c_dictIDFlag    = 0                        (no in-frame dict ID)
//	ZSTD_c_checksumFlag  = 0                        (no checksum)
//	compression level    = ZSTD_CLEVEL_DEFAULT (3)
//
// The dictionary is selected per block as min(block_num/1_000_000, last_dict).
//
// We need cgo because klauspost/compress/zstd does not expose magicless mode
// or zstd compression levels above SpeedBestCompression — neither of which
// reproduces the on-disk byte sequence hived emits.

package zstd

/*
#cgo pkg-config: libzstd
#include <stdlib.h>
#include <string.h>
#include <zstd.h>

// ZSTD_f_zstd1_magicless lives behind ZSTD_STATIC_LINKING_ONLY; if the
// installed libzstd headers were built without it, we assert at compile
// time. Most distributions (including 1.4.8) ship the experimental API
// header guarded by this macro.
#define ZSTD_STATIC_LINKING_ONLY
#include <zstd.h>

// Wrapper that mirrors hived's compress_block_zstd_helper. dictData/dictSize
// may be NULL/0 for "no dictionary".
//
// dst must point to a buffer of at least ZSTD_compressBound(srcSize). Returns
// compressed size or a zstd error code (use ZSTD_isError on the result).
static size_t go_compress_block(
    void *dst, size_t dst_capacity,
    const void *src, size_t src_size,
    const void *dict_data, size_t dict_size,
    int compression_level)
{
    ZSTD_CCtx *cctx = ZSTD_createCCtx();
    if (cctx == NULL) {
        return (size_t)-1;
    }

    size_t rc;

    // Set parameters BEFORE loading the dict, mirroring hived's order.
    rc = ZSTD_CCtx_setParameter(cctx, ZSTD_c_compressionLevel, compression_level);
    if (ZSTD_isError(rc)) { ZSTD_freeCCtx(cctx); return rc; }

    if (dict_data != NULL && dict_size > 0) {
        // ZSTD_CCtx_loadDictionary takes raw dict bytes (a "by reference"
        // variant via ZSTD_CCtx_loadDictionary_byReference exists; hived uses
        // ZSTD_CCtx_refCDict with a pre-built CDict but the byte output is
        // identical for matched parameters).
        rc = ZSTD_CCtx_loadDictionary(cctx, dict_data, dict_size);
        if (ZSTD_isError(rc)) { ZSTD_freeCCtx(cctx); return rc; }
    }

    rc = ZSTD_CCtx_setParameter(cctx, ZSTD_c_format, ZSTD_f_zstd1_magicless);
    if (ZSTD_isError(rc)) { ZSTD_freeCCtx(cctx); return rc; }
    rc = ZSTD_CCtx_setParameter(cctx, ZSTD_c_contentSizeFlag, 0);
    if (ZSTD_isError(rc)) { ZSTD_freeCCtx(cctx); return rc; }
    rc = ZSTD_CCtx_setParameter(cctx, ZSTD_c_dictIDFlag, 0);
    if (ZSTD_isError(rc)) { ZSTD_freeCCtx(cctx); return rc; }
    rc = ZSTD_CCtx_setParameter(cctx, ZSTD_c_checksumFlag, 0);
    if (ZSTD_isError(rc)) { ZSTD_freeCCtx(cctx); return rc; }

    rc = ZSTD_compress2(cctx, dst, dst_capacity, src, src_size);
    ZSTD_freeCCtx(cctx);
    return rc;
}

// Persistent-CCtx path: hived (and the old go_compress_block) re-created the
// CCtx and re-digested the trained dictionary on EVERY block —
// ZSTD_CCtx_loadDictionary digests the dict each call (~ms), which caps a
// compressed-write sync at a few hundred blocks/sec. A CCtx's dictionary and
// parameters are STICKY across ZSTD_compress2 calls (only ZSTD_CCtx_reset
// clears them), so we create the CCtx once, load each dictionary once (on
// dict-number change — i.e. ~once per 1M blocks), and reuse it. Byte output is
// identical to the per-call path for matched parameters (guarded by
// TestByteIdenticalAgainstMainnet).
static ZSTD_CCtx* go_make_persistent_cctx(int level) {
    ZSTD_CCtx* cctx = ZSTD_createCCtx();
    if (cctx == NULL) return NULL;
    if (ZSTD_isError(ZSTD_CCtx_setParameter(cctx, ZSTD_c_compressionLevel, level)) ||
        ZSTD_isError(ZSTD_CCtx_setParameter(cctx, ZSTD_c_format, ZSTD_f_zstd1_magicless)) ||
        ZSTD_isError(ZSTD_CCtx_setParameter(cctx, ZSTD_c_contentSizeFlag, 0)) ||
        ZSTD_isError(ZSTD_CCtx_setParameter(cctx, ZSTD_c_dictIDFlag, 0)) ||
        ZSTD_isError(ZSTD_CCtx_setParameter(cctx, ZSTD_c_checksumFlag, 0))) {
        ZSTD_freeCCtx(cctx);
        return NULL;
    }
    return cctx;
}
// go_cctx_load_dict digests dict into cctx (sticky for following compresses).
static size_t go_cctx_load_dict(ZSTD_CCtx* cctx, const void* dict, size_t dsize) {
    return ZSTD_CCtx_loadDictionary(cctx, dict, dsize);
}
static size_t go_cctx_compress(ZSTD_CCtx* cctx, void* dst, size_t cap, const void* src, size_t ssize) {
    return ZSTD_compress2(cctx, dst, cap, src, ssize);
}
static void go_free_persistent_cctx(ZSTD_CCtx* c) { if (c != NULL) ZSTD_freeCCtx(c); }

static int go_zstd_is_error(size_t rc) { return ZSTD_isError(rc); }
static const char* go_zstd_error_name(size_t rc) { return ZSTD_getErrorName(rc); }
static size_t go_zstd_compress_bound(size_t s) { return ZSTD_compressBound(s); }

// Hived uses the no-arg form of compress_block_zstd which defaults to
// fc::optional(), then helper applies value_or(ZSTD_CLEVEL_DEFAULT).
static int go_zstd_default_clevel(void) { return ZSTD_CLEVEL_DEFAULT; }
*/
import "C"
import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

// DefaultCompressionLevel returns the level the writer uses by default.
//
// Hived has TWO defaults: ZSTD_CLEVEL_DEFAULT (3) for in-process compression
// during live sync (full_block_type::compress_block, no level argument), and
// 15 for offline preparation via `programs/util/compress_block_log` (the tool
// that produces the canonical compressed mainnet block_logs distributed for
// replay).
//
// The on-disk bytes shipped with mainnet block_logs are level-15. Empirically,
// re-encoding those bytes at level 15 with libzstd 1.4.8 reproduces them
// byte-identically (TestByteIdenticalAgainstMainnet). Level 3 does not. We
// pick 15 here so OpenForWrite(path, true) generates a block_log that is
// indistinguishable from the canonical compressed file.
func DefaultCompressionLevel() int { return 15 }

// LibraryDefaultCompressionLevel exposes the value of ZSTD_CLEVEL_DEFAULT
// from the underlying libzstd. Diagnostic only.
func LibraryDefaultCompressionLevel() int { return int(C.go_zstd_default_clevel()) }

// LastAvailableDictNumber returns the highest dictionary number embedded in
// this build, or (0, false) if no dictionaries are bundled.
func LastAvailableDictNumber() (uint8, bool) {
	if len(rawDicts) == 0 {
		return 0, false
	}
	var max uint8
	for n := range rawDicts {
		if n > max {
			max = n
		}
	}
	return max, true
}

// BestDictNumberForBlock returns the dictionary number hived would choose for
// blockNum: min(blockNum/1_000_000, last_available_dict). If no dictionaries
// are embedded, returns (0, false) and callers should compress without a
// dictionary.
func BestDictNumberForBlock(blockNum uint32) (uint8, bool) {
	last, ok := LastAvailableDictNumber()
	if !ok {
		return 0, false
	}
	cand := uint32(blockNum / 1_000_000)
	if cand > uint32(last) {
		cand = uint32(last)
	}
	return uint8(cand), true
}

// Compressor encodes blocks the same way hived does on disk. Safe for
// concurrent use; the underlying zstd CCtx is created per call (cheap; the
// expensive work is dict load, which is amortized via the cached
// decompressed-dict buffer).
type Compressor struct {
	level int
	mu    sync.Mutex
	// scratch is reused between compress calls within a single goroutine
	// (guarded by mu). Most blocks are <100KB; we grow as needed.
	scratch []byte
	// cctx is a persistent compression context (lazily created on first
	// dict-based Compress). loadedDict is the dictionary number currently
	// digested into it, or -1 if none. Reusing the cctx across blocks avoids
	// re-digesting the dictionary every call.
	cctx       *C.ZSTD_CCtx
	loadedDict int
}

// newCompressor builds a Compressor and arms a finalizer to free its
// persistent CCtx (created lazily) when the Compressor is collected.
func newCompressor(level int) *Compressor {
	c := &Compressor{level: level, loadedDict: -1}
	runtime.SetFinalizer(c, func(c *Compressor) {
		if c.cctx != nil {
			C.go_free_persistent_cctx(c.cctx)
			c.cctx = nil
		}
	})
	return c
}

// NewCompressor returns a Compressor configured with hived's default
// compression level (CLEVEL_DEFAULT = 3).
func NewCompressor() *Compressor { return newCompressor(DefaultCompressionLevel()) }

// NewCompressorWithLevel allows overriding the compression level. Useful for
// the parity self-tests (mainnet block_log was historically written at
// level 3, but other deployments may use 15 — see the comment in
// libraries/chain/block_log.cpp).
func NewCompressorWithLevel(level int) *Compressor { return newCompressor(level) }

// Compress encodes src using the dictionary identified by dictNum and
// returns the magicless zstd payload as it would appear on disk. Reuses a
// persistent CCtx, re-digesting the dictionary only when dictNum changes.
func (c *Compressor) Compress(src []byte, dictNum uint8) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cctx == nil {
		c.cctx = C.go_make_persistent_cctx(C.int(c.level))
		if c.cctx == nil {
			return nil, fmt.Errorf("blocklog/zstd: ZSTD_createCCtx failed")
		}
		c.loadedDict = -1
	}
	if int(dictNum) != c.loadedDict {
		dict, err := getDecompressedDict(dictNum)
		if err != nil {
			return nil, err
		}
		var dictPtr unsafe.Pointer
		if len(dict) > 0 {
			dictPtr = unsafe.Pointer(&dict[0])
		}
		rc := C.go_cctx_load_dict(c.cctx, dictPtr, C.size_t(len(dict)))
		runtime.KeepAlive(dict)
		if C.go_zstd_is_error(rc) != 0 {
			return nil, fmt.Errorf("blocklog/zstd: load dictionary %d: %s",
				dictNum, C.GoString(C.go_zstd_error_name(rc)))
		}
		c.loadedDict = int(dictNum)
	}

	bound := int(C.go_zstd_compress_bound(C.size_t(len(src))))
	if cap(c.scratch) < bound {
		c.scratch = make([]byte, bound)
	} else {
		c.scratch = c.scratch[:bound]
	}
	var srcPtr unsafe.Pointer
	if len(src) > 0 {
		srcPtr = unsafe.Pointer(&src[0])
	}
	rc := C.go_cctx_compress(c.cctx,
		unsafe.Pointer(&c.scratch[0]), C.size_t(len(c.scratch)),
		srcPtr, C.size_t(len(src)))
	if C.go_zstd_is_error(rc) != 0 {
		return nil, fmt.Errorf("blocklog/zstd: ZSTD_compress2 failed: %s",
			C.GoString(C.go_zstd_error_name(rc)))
	}
	out := make([]byte, int(rc))
	copy(out, c.scratch[:int(rc)])
	return out, nil
}

// CompressNoDict encodes src without a trained dictionary.
func (c *Compressor) CompressNoDict(src []byte) ([]byte, error) {
	return c.compressWith(src, nil)
}

func (c *Compressor) compressWith(src, dict []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	bound := int(C.go_zstd_compress_bound(C.size_t(len(src))))
	if cap(c.scratch) < bound {
		c.scratch = make([]byte, bound)
	} else {
		c.scratch = c.scratch[:bound]
	}

	var srcPtr unsafe.Pointer
	if len(src) > 0 {
		srcPtr = unsafe.Pointer(&src[0])
	}
	var dictPtr unsafe.Pointer
	var dictSize C.size_t
	if len(dict) > 0 {
		dictPtr = unsafe.Pointer(&dict[0])
		dictSize = C.size_t(len(dict))
	}

	rc := C.go_compress_block(
		unsafe.Pointer(&c.scratch[0]),
		C.size_t(len(c.scratch)),
		srcPtr,
		C.size_t(len(src)),
		dictPtr,
		dictSize,
		C.int(c.level),
	)
	if C.go_zstd_is_error(rc) != 0 {
		return nil, fmt.Errorf("blocklog/zstd: ZSTD_compress2 failed: %s",
			C.GoString(C.go_zstd_error_name(rc)))
	}
	out := make([]byte, int(rc))
	copy(out, c.scratch[:int(rc)])
	return out, nil
}
