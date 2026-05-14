// cgo_decoder.go — libzstd-backed block decoder. Mirrors the encoder's
// magicless / no-checksum / no-dict-ID frame layout so the on-disk
// payload can be fed directly without reframing.
//
// Hived's block_log writes blocks with ZSTD_f_zstd1_magicless: the
// standard 4-byte zstd magic is stripped from every payload. The
// klauspost/compress/zstd library can't decode magicless frames, so
// the pure-Go path in zstd.go does an expensive reframe (prepend
// magic + inject fake DictionaryID) before each DecodeAll call.
//
// libzstd CAN decode magicless directly via
// ZSTD_d_format = ZSTD_f_zstd1_magicless on the DCtx. Each Decompressor
// keeps a per-dict DCtx pre-loaded with the trained dictionary; per-
// block decode is then ZSTD_decompressDCtx into a Go-owned output
// buffer. The CGO call overhead (~150ns) is dwarfed by the decode
// itself (~3-10µs per block).
//
// On 8M-block replay, the pure-Go decoder spent ~22s (18% of CPU) in
// klauspost zstd internals + reframe + memmove. libzstd is typically
// 2-3× faster on small magicless payloads with a warm dict.

package zstd

/*
#cgo pkg-config: libzstd
#include <stdlib.h>
#include <string.h>
#define ZSTD_STATIC_LINKING_ONLY
#include <zstd.h>

// Create a DCtx pre-configured for hived's magicless format and (if
// dictData != NULL) with the trained dictionary loaded.
//
// Returns (DCtx*, error_code). On error_code != 0, DCtx is NULL.
static ZSTD_DCtx* go_make_dctx(const void *dict_data, size_t dict_size, size_t *err) {
    ZSTD_DCtx *dctx = ZSTD_createDCtx();
    if (dctx == NULL) {
        *err = (size_t)-1;
        return NULL;
    }
    size_t rc = ZSTD_DCtx_setParameter(dctx, ZSTD_d_format, ZSTD_f_zstd1_magicless);
    if (ZSTD_isError(rc)) {
        ZSTD_freeDCtx(dctx);
        *err = rc;
        return NULL;
    }
    if (dict_data != NULL && dict_size > 0) {
        // Load by reference: caller (Go side) must keep dict_data alive
        // for the DCtx's lifetime. We hold the patched dict slice on
        // the cgoDecoder struct, so this invariant is satisfied.
        rc = ZSTD_DCtx_loadDictionary_byReference(dctx, dict_data, dict_size);
        if (ZSTD_isError(rc)) {
            ZSTD_freeDCtx(dctx);
            *err = rc;
            return NULL;
        }
    }
    *err = 0;
    return dctx;
}

// Decompress payload (magicless) into dst (pre-sized by caller).
// Returns decompressed size on success, or a zstd error code (test
// with ZSTD_isError).
static size_t go_decompress_dctx(ZSTD_DCtx *dctx, void *dst, size_t dst_capacity, const void *src, size_t src_size) {
    return ZSTD_decompressDCtx(dctx, dst, dst_capacity, src, src_size);
}

static void go_free_dctx(ZSTD_DCtx *dctx) {
    if (dctx != NULL) ZSTD_freeDCtx(dctx);
}

static size_t go_dctx_out_size_bound(const void *src, size_t src_size) {
    // ZSTD_getFrameContentSize reads the optional decoded-size field from
    // the frame header. Magicless hived frames have ZSTD_c_contentSizeFlag=0
    // (no decoded size embedded), so this returns ZSTD_CONTENTSIZE_UNKNOWN.
    // We can't statically bound the output; pick a generous default and let
    // the caller grow if needed. Hived blocks decompress to ≤256 KB.
    (void)src; (void)src_size;
    return 256 * 1024;
}

static int go_zstd_is_err(size_t rc) { return ZSTD_isError(rc); }
static const char* go_zstd_err_name(size_t rc) { return ZSTD_getErrorName(rc); }
*/
import "C"
import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

// cgoDecoder wraps a single libzstd DCtx for a specific dict number.
// DCtx is NOT thread-safe, but each cgoDecoder is acquired from a
// per-dict sync.Pool and used by exactly one goroutine at a time,
// then returned. So no mutex is needed at this level — the pool
// itself provides the exclusion.
type cgoDecoder struct {
	dctx     *C.ZSTD_DCtx
	dictKeep []byte // pinned dict bytes (held to satisfy by-reference load)
}

// cgoDecoderPool holds a per-Decompressor cache of cgoDecoders.
// Keyed by dict number; each slot is a sync.Pool of DCtxs so multiple
// peer goroutines can decompress with the same dictionary concurrently
// instead of serialising through a single per-dict mutex. DCtxs are
// returned to the pool after each call and reused.
type cgoDecoderPool struct {
	mu    sync.Mutex
	pools map[uint8]*cgoDictPool
}

// cgoDictPool is the per-dict shard. It uses a sync.Pool so idle
// DCtxs get reaped over time, and dictKeep stays alive on the parent
// so DCtxs that hold a by-reference dict pointer remain valid.
type cgoDictPool struct {
	num      uint8
	dictKeep []byte
	pool     sync.Pool
}

func newCGODecoderPool() *cgoDecoderPool {
	return &cgoDecoderPool{pools: map[uint8]*cgoDictPool{}}
}

func (p *cgoDecoderPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, dp := range p.pools {
		for {
			v := dp.pool.Get()
			if v == nil {
				break
			}
			C.go_free_dctx(v.(*cgoDecoder).dctx)
		}
		delete(p.pools, k)
	}
}

func (p *cgoDecoderPool) decoderFor(num uint8) (*cgoDecoder, error) {
	p.mu.Lock()
	dp, ok := p.pools[num]
	if !ok {
		var dictKeep []byte
		if num != noDictKey {
			raw, err := getDecompressedDict(num)
			if err != nil {
				p.mu.Unlock()
				return nil, err
			}
			dictKeep = append([]byte(nil), raw...)
		}
		dp = &cgoDictPool{num: num, dictKeep: dictKeep}
		p.pools[num] = dp
	}
	p.mu.Unlock()

	if v := dp.pool.Get(); v != nil {
		return v.(*cgoDecoder), nil
	}
	// Pool empty — create a fresh DCtx.
	var dictData unsafe.Pointer
	var dictSize C.size_t
	if len(dp.dictKeep) > 0 {
		dictData = unsafe.Pointer(&dp.dictKeep[0])
		dictSize = C.size_t(len(dp.dictKeep))
	}
	var errCode C.size_t
	dctx := C.go_make_dctx(dictData, dictSize, &errCode)
	if dctx == nil {
		return nil, fmt.Errorf("blocklog/zstd: ZSTD_createDCtx (dict %d): %s",
			num, C.GoString(C.go_zstd_err_name(errCode)))
	}
	return &cgoDecoder{dctx: dctx, dictKeep: dp.dictKeep}, nil
}

// release returns a decoder to its per-dict pool for reuse. Called by
// decompress on every successful (or error) return.
func (p *cgoDecoderPool) release(num uint8, d *cgoDecoder) {
	p.mu.Lock()
	dp, ok := p.pools[num]
	p.mu.Unlock()
	if !ok {
		C.go_free_dctx(d.dctx)
		return
	}
	dp.pool.Put(d)
}

// decompress runs ZSTD_decompressDCtx on a magicless payload. dst is
// the caller-supplied output buffer (a fresh slice or pooled). On
// dst capacity overflow, retry with a doubled buffer.
func (d *cgoDecoder) decompress(payload []byte, dst []byte) ([]byte, error) {
	if cap(dst) < 8192 {
		dst = make([]byte, 0, 65536)
	}
	for attempt := 0; attempt < 3; attempt++ {
		dst = dst[:cap(dst)]
		var srcPtr unsafe.Pointer
		if len(payload) > 0 {
			srcPtr = unsafe.Pointer(&payload[0])
		}
		rc := C.go_decompress_dctx(d.dctx,
			unsafe.Pointer(&dst[0]), C.size_t(len(dst)),
			srcPtr, C.size_t(len(payload)))
		if C.go_zstd_is_err(rc) != 0 {
			errMsg := C.GoString(C.go_zstd_err_name(rc))
			// `Destination buffer is too small`: ZSTD_isError returns
			// non-zero but we'd like to retry with a larger buffer.
			if attempt < 2 && (errMsg == "Destination buffer is too small" ||
				errMsg == "Destination buffer too small") {
				newCap := cap(dst) * 2
				if newCap < 65536 {
					newCap = 65536
				}
				dst = make([]byte, 0, newCap)
				continue
			}
			return nil, fmt.Errorf("blocklog/zstd: ZSTD_decompressDCtx: %s", errMsg)
		}
		out := dst[:int(rc)]
		runtime.KeepAlive(payload)
		runtime.KeepAlive(d.dictKeep)
		return out, nil
	}
	return nil, fmt.Errorf("blocklog/zstd: decompress failed after retries")
}
