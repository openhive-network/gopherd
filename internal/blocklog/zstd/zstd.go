// Package zstd provides block-level zstd decompression with the dictionary
// scheme hived uses on disk.
//
// Hived embeds N zstd-trained dictionaries — one per "block million" — under
// libraries/chain/compression_dictionaries/. Each on-disk dictionary file is
// itself zstd-compressed; at runtime, hived first decompresses the dictionary
// using vanilla zstd, then uses the decompressed bytes as the dictionary
// parameter for decoding blocks.
//
// We mirror that behavior here: the embed-merged .zst files are read at
// package init, lazily decompressed into raw dictionaries on first use, and
// cached. A pool of *zstd.Decoder is kept per dictionary so concurrent
// callers don't fight over a single decoder state.
package zstd

import (
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

//go:embed dicts/*.zst
var rawDictFS embed.FS

// rawDicts holds the embedded compressed-dictionary bytes, keyed by
// dictionary number. Populated at package init.
var rawDicts map[uint8][]byte

func init() {
	rawDicts = make(map[uint8][]byte)
	entries, err := fs.ReadDir(rawDictFS, "dicts")
	if err != nil {
		panic("blocklog/zstd: failed to read embedded dicts/: " + err.Error())
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "dict_") || !strings.HasSuffix(name, ".zst") {
			continue
		}
		numStr := strings.TrimSuffix(strings.TrimPrefix(name, "dict_"), ".zst")
		n, err := strconv.ParseUint(numStr, 10, 8)
		if err != nil {
			continue
		}
		b, err := fs.ReadFile(rawDictFS, "dicts/"+name)
		if err != nil {
			panic("blocklog/zstd: failed to read embedded dict " + name + ": " + err.Error())
		}
		rawDicts[uint8(n)] = b
	}
}

// AvailableDictNumbers returns the dictionary numbers the package can decode.
// Useful for diagnostics — extends as more dict files are bundled.
func AvailableDictNumbers() []uint8 {
	out := make([]uint8, 0, len(rawDicts))
	for k := range rawDicts {
		out = append(out, k)
	}
	return out
}

// RawDictBytes returns the embedded compressed-dictionary bytes for num.
// Diagnostic only.
func RawDictBytes(num uint8) ([]byte, error) {
	b, ok := rawDicts[num]
	if !ok {
		return nil, fmt.Errorf("blocklog/zstd: no embedded dict %d", num)
	}
	return b, nil
}

// DecompressedDictBytes returns the (lazily) decompressed dictionary bytes
// for num. Diagnostic only.
func DecompressedDictBytes(num uint8) ([]byte, error) { return getDecompressedDict(num) }

// decompressedDicts caches the raw (decompressed) dictionary bytes per number.
// We decompress lazily because most workloads only touch a small subset of
// dictionaries (typically 1–10 covering the chain region they're scanning).
var (
	decompressedDictsMu sync.Mutex
	decompressedDicts   = map[uint8][]byte{}
)

func getDecompressedDict(num uint8) ([]byte, error) {
	decompressedDictsMu.Lock()
	defer decompressedDictsMu.Unlock()

	if d, ok := decompressedDicts[num]; ok {
		return d, nil
	}
	raw, ok := rawDicts[num]
	if !ok {
		return nil, fmt.Errorf("blocklog/zstd: dictionary %d not embedded (rebuild with the corresponding .zst file added under dicts/)", num)
	}
	// Vanilla zstd decode — no dictionary needed for the dict-of-dict step.
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	out, err := dec.DecodeAll(raw, nil)
	if err != nil {
		return nil, fmt.Errorf("blocklog/zstd: decompress dictionary %d: %w", num, err)
	}
	decompressedDicts[num] = out
	return out, nil
}

// Decompressor wraps a per-dict-number cache of zstd decoders. It is safe for
// concurrent use; obtain one via NewDecompressor and reuse across blocks.
type Decompressor struct {
	mu       sync.Mutex
	decoders map[uint8]*zstd.Decoder
	// framedPool reuses the re-framed (magic + DictID-patched header +
	// payload) scratch buffer across Decompress calls. Each replay does
	// ~one decompress per block; without the pool reframe allocates a
	// fresh slice every call (1.07 GB cum on a 4M-block run).
	framedPool sync.Pool
}

// NewDecompressor returns a fresh Decompressor.
func NewDecompressor() *Decompressor {
	d := &Decompressor{decoders: map[uint8]*zstd.Decoder{}}
	d.framedPool.New = func() any { b := make([]byte, 0, 8192); return &b }
	return d
}

// Close releases all decoder resources.
func (d *Decompressor) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, dec := range d.decoders {
		dec.Close()
		delete(d.decoders, k)
	}
}

// magic is the standard zstd frame magic. Hived writes blocks with
// ZSTD_f_zstd1_magicless — i.e. the magic is *stripped* from the on-disk
// payload to save 4 bytes per block. We re-prepend it before handing data
// to klauspost/compress/zstd, which only speaks the standard framed format.
var magic = [4]byte{0x28, 0xb5, 0x2f, 0xfd}

// fakeDictIDBase is OR'd with the dictionary number to produce a per-dict
// fake DictionaryID we register dicts under. We need to fake an ID because:
//
//  1. Hived compresses blocks with the Dictionary_ID_Flag clear, so the
//     frame's DictionaryID is 0.
//  2. klauspost's loadDict() rejects ID==0 trained-format dicts outright,
//     so we cannot register our trained dict under ID 0.
//
// Workaround: rewrite each block's frame header to add a 4-byte
// DictionaryID equal to fakeDictID(dictNum), and patch the trained-dict
// bytes' embedded ID to match. This lets klauspost match dict↔frame by ID
// the way its API requires.
//
// The chosen base avoids collisions with the real dict IDs hived embeds
// (which are small uint8s — 1, 2, 3, ..., or large random values for dict 0).
const fakeDictIDBase uint32 = 0xC0F0_FE00

func fakeDictID(dictNum uint8) uint32 { return fakeDictIDBase | uint32(dictNum) }

// reframe prepends the zstd magic and injects a fake 4-byte DictionaryID
// into the frame header so the decoder picks up our trained dict.
//
// The on-disk frame begins (per RFC 8478 §3.1.1) with:
//
//	Frame_Header_Descriptor (1 byte)
//	[Window_Descriptor]              if Single_Segment_Flag == 0   (1 byte)
//	[Dictionary_ID]                  per FHD bits 0–1 (0/1/2/4)
//	[Frame_Content_Size]             per FHD bits 6–7 + SSF
//
// Hived's blocks always have DictionaryID size 0 (FHD bits 0–1 == 00), so
// we splice 4 bytes of dictID after WD/before FCS and set FHD bits 0–1 = 11.
// reframeInto rebuilds payload as a complete zstd frame in dst (re-used
// across calls when caller passes a pooled scratch buffer). Returns dst
// extended with the framed bytes. Behavior mirrors reframe; the
// indirection lets callers avoid the per-call alloc.
func reframeInto(dst, payload []byte, dictNum uint8) ([]byte, error) {
	dst = dst[:0]
	if len(payload) < 1 {
		return nil, errors.New("blocklog/zstd: empty compressed payload")
	}
	fhd := payload[0]
	if fhd&0x03 != 0 {
		dst = append(dst, magic[:]...)
		dst = append(dst, payload...)
		return dst, nil
	}
	singleSegment := (fhd >> 5) & 1
	wdSize := 1
	if singleSegment == 1 {
		wdSize = 0
	}
	var fcsSize int
	switch fhd >> 6 {
	case 0:
		if singleSegment == 1 {
			fcsSize = 1
		}
	case 1:
		fcsSize = 2
	case 2:
		fcsSize = 4
	case 3:
		fcsSize = 8
	}
	headerEnd := 1 + wdSize + fcsSize
	if headerEnd > len(payload) {
		return nil, fmt.Errorf("blocklog/zstd: frame header truncated (need %d, have %d)", headerEnd, len(payload))
	}
	dst = append(dst, magic[:]...)
	newFHD := (fhd &^ 0x03) | 0x03
	dst = append(dst, newFHD)
	dst = append(dst, payload[1:1+wdSize]...)
	var idBytes [4]byte
	binary.LittleEndian.PutUint32(idBytes[:], fakeDictID(dictNum))
	dst = append(dst, idBytes[:]...)
	dst = append(dst, payload[1+wdSize:1+wdSize+fcsSize]...)
	dst = append(dst, payload[1+wdSize+fcsSize:]...)
	return dst, nil
}

func reframe(payload []byte, dictNum uint8) ([]byte, error) {
	if len(payload) < 1 {
		return nil, errors.New("blocklog/zstd: empty compressed payload")
	}
	fhd := payload[0]
	if fhd&0x03 != 0 {
		// Frame already has a DictionaryID — pass through with magic only.
		out := make([]byte, len(payload)+4)
		copy(out[:4], magic[:])
		copy(out[4:], payload)
		return out, nil
	}
	singleSegment := (fhd >> 5) & 1
	wdSize := 1
	if singleSegment == 1 {
		wdSize = 0
	}
	var fcsSize int
	switch fhd >> 6 {
	case 0:
		if singleSegment == 1 {
			fcsSize = 1
		}
	case 1:
		fcsSize = 2
	case 2:
		fcsSize = 4
	case 3:
		fcsSize = 8
	}
	headerEnd := 1 + wdSize + fcsSize // dictID size is 0 in source frame
	if headerEnd > len(payload) {
		return nil, fmt.Errorf("blocklog/zstd: frame header truncated (need %d, have %d)", headerEnd, len(payload))
	}

	out := make([]byte, 0, 4+len(payload)+4)
	out = append(out, magic[:]...)
	newFHD := (fhd &^ 0x03) | 0x03 // set Dictionary_ID_Flag = 11 → 4 bytes
	out = append(out, newFHD)
	out = append(out, payload[1:1+wdSize]...)
	var idBytes [4]byte
	binary.LittleEndian.PutUint32(idBytes[:], fakeDictID(dictNum))
	out = append(out, idBytes[:]...)
	out = append(out, payload[1+wdSize:1+wdSize+fcsSize]...)
	out = append(out, payload[1+wdSize+fcsSize:]...)
	return out, nil
}

// Decompress decompresses block-payload bytes using the dictionary identified
// by dictNum. dst, if non-nil, is appended to and returned for reuse;
// otherwise a fresh slice is allocated.
//
// payload is expected to be in hived's on-disk format — magicless and with no
// in-frame DictionaryID.
func (d *Decompressor) Decompress(payload []byte, dictNum uint8, dst []byte) ([]byte, error) {
	dec, err := d.decoderFor(dictNum)
	if err != nil {
		return nil, err
	}
	bufPtr := d.framedPool.Get().(*[]byte)
	framed, err := reframeInto(*bufPtr, payload, dictNum)
	if err != nil {
		*bufPtr = (*bufPtr)[:0]
		d.framedPool.Put(bufPtr)
		return nil, err
	}
	out, err := dec.DecodeAll(framed, dst)
	*bufPtr = framed[:0]
	d.framedPool.Put(bufPtr)
	if err != nil {
		return nil, fmt.Errorf("blocklog/zstd: decode block (dict %d, %d compressed bytes): %w", dictNum, len(payload), err)
	}
	return out, nil
}

// DecompressNoDict decompresses payload that wasn't trained against a
// dictionary. Hive zstd-compressed blocks generally do carry a dictionary,
// but this is kept as the symmetric API in case dict-less compressed blocks
// are encountered.
func (d *Decompressor) DecompressNoDict(payload, dst []byte) ([]byte, error) {
	dec, err := d.decoderFor(noDictKey)
	if err != nil {
		return nil, err
	}
	// No dict means we don't inject — just prepend magic.
	out := make([]byte, len(payload)+4)
	copy(out[:4], magic[:])
	copy(out[4:], payload)
	return dec.DecodeAll(out, dst)
}

const noDictKey uint8 = 255 // sentinel for the "no dictionary" decoder slot

var errInvalidDictKey = errors.New("blocklog/zstd: invalid dictionary key")

func (d *Decompressor) decoderFor(num uint8) (*zstd.Decoder, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if dec, ok := d.decoders[num]; ok {
		return dec, nil
	}

	if num == noDictKey {
		dec, err := zstd.NewReader(nil)
		if err != nil {
			return nil, err
		}
		d.decoders[num] = dec
		return dec, nil
	}

	rawDict, err := getDecompressedDict(num)
	if err != nil {
		return nil, err
	}
	// Patch the trained-dictionary's embedded 4-byte ID (offset 4..8 — right
	// after the 0x37A430EC dict magic) to fakeDictID(num). reframe()
	// inserts the same fake ID into each block's frame header so klauspost
	// matches the dict by ID at decode time.
	patched := append([]byte(nil), rawDict...)
	if len(patched) < 8 {
		return nil, fmt.Errorf("blocklog/zstd: dict %d too short (%d bytes)", num, len(patched))
	}
	binary.LittleEndian.PutUint32(patched[4:8], fakeDictID(num))
	dec, err := zstd.NewReader(nil, zstd.WithDecoderDicts(patched))
	if err != nil {
		return nil, fmt.Errorf("blocklog/zstd: build decoder for dict %d: %w", num, err)
	}
	d.decoders[num] = dec
	return dec, nil
}
