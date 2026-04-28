package fcraw

import "errors"

// Hive's "varint" types come from libraries/fc/include/fc/io/varint.hpp:
//
//   - unsigned_int : 32-bit unsigned, encoded as LEB128 (low 7 bits per byte,
//                    high bit set means "another byte follows"). Zero is one
//                    0x00 byte. Maximum encoded length is 5 bytes (matching
//                    the C++ unpack guard `limit < 5`).
//
//   - signed_int   : 32-bit signed, ZigZag-encoded then LEB128 — same wire
//                    format protobuf uses for sint32. The encode side is the
//                    canonical (v<<1)^(v>>31); the C++ decode has historical
//                    quirks but the wire format is unambiguous.
//
// We expose helpers operating directly on byte slices so Writer/Reader stay
// allocation-free in the hot path.

// MaxVarUintLen is the maximum number of bytes a 32-bit unsigned varint can
// occupy on the wire (matches the C++ `limit < 5` guard in fc::raw::unpack).
const MaxVarUintLen = 5

var (
	errVarUintTooLong = errors.New("fcraw: varint exceeds 5 bytes")
	errVarUintEOF     = errors.New("fcraw: unexpected EOF in varint")
)

// AppendVarUint encodes v as an LEB128 varint and appends it to dst.
// Matches fc::raw::pack(unsigned_int) byte-for-byte.
func AppendVarUint(dst []byte, v uint32) []byte {
	for {
		b := byte(v) & 0x7f
		v >>= 7
		if v != 0 {
			dst = append(dst, b|0x80)
			continue
		}
		dst = append(dst, b)
		return dst
	}
}

// ReadVarUint decodes an LEB128 varint from src starting at offset off.
// Returns the value, the number of bytes consumed, and an error if the
// encoding is malformed or src is too short.
func ReadVarUint(src []byte, off int) (v uint32, n int, err error) {
	var shift uint
	for n < MaxVarUintLen {
		if off+n >= len(src) {
			return 0, 0, errVarUintEOF
		}
		b := src[off+n]
		v |= uint32(b&0x7f) << shift
		n++
		if b&0x80 == 0 {
			return v, n, nil
		}
		shift += 7
	}
	return 0, 0, errVarUintTooLong
}

// AppendVarInt encodes v as ZigZag + LEB128 and appends it to dst.
// Matches fc::raw::pack(signed_int) byte-for-byte.
func AppendVarInt(dst []byte, v int32) []byte {
	zig := uint32(v<<1) ^ uint32(v>>31)
	return AppendVarUint(dst, zig)
}

// ReadVarInt decodes a ZigZag varint from src starting at off.
func ReadVarInt(src []byte, off int) (v int32, n int, err error) {
	zig, n, err := ReadVarUint(src, off)
	if err != nil {
		return 0, 0, err
	}
	v = int32(zig>>1) ^ -int32(zig&1)
	return v, n, nil
}
