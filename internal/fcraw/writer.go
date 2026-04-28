// Package fcraw implements the FC binary serialization format used by hived
// on the wire and on disk. It is a Go-idiomatic replacement for FC's
// `fc::raw::pack` / `fc::raw::unpack` template machinery, with the same
// byte-for-byte output.
//
// Wire format invariants (cross-checked with libraries/fc/include/fc/io/raw.hpp):
//
//   - Fixed-size integers are little-endian. Hive only runs on LE hosts;
//     the C++ code calls s.write((char*)&v, sizeof(v)) directly, locking LE
//     into the wire format.
//   - bool is one byte (0 or 1; any other value is a decode error).
//   - Variable-length integers (`unsigned_int`, `signed_int`) are LEB128 /
//     ZigZag+LEB128. See varint.go.
//   - Strings and `std::vector<char>` are: unsigned_int length, then raw bytes.
//   - Containers (vector<T>, set<T>, map<K,V>): unsigned_int count, then items.
//   - Pairs are concatenations.
//   - `optional<T>` / `std::optional<T>`: bool flag, then value if present.
//   - `fc::array<T, N>` (fixed-length): raw bytes, no length prefix.
//   - time_point_sec: 4 bytes, uint32 LE, seconds since epoch.
//   - time_point: 8 bytes, uint64 LE, microseconds since epoch.
//
// The Writer is a value type wrapping a growable byte slice. It is not safe
// for concurrent use; callers should use one Writer per goroutine and reuse
// via Reset() across blocks (sync.Pool is appropriate in the apply hot path).
package fcraw

import "encoding/binary"

// Writer accumulates bytes in FC binary form into an internal slice.
type Writer struct {
	buf []byte
}

// NewWriter returns a Writer with an empty buffer.
func NewWriter() *Writer { return &Writer{} }

// NewWriterWithCap returns a Writer with the given initial capacity.
func NewWriterWithCap(n int) *Writer { return &Writer{buf: make([]byte, 0, n)} }

// Bytes returns the current buffer contents. The returned slice aliases the
// Writer's internal storage; callers that need a stable copy must clone.
func (w *Writer) Bytes() []byte { return w.buf }

// Len returns the number of bytes written so far.
func (w *Writer) Len() int { return len(w.buf) }

// Reset clears the buffer for reuse, retaining capacity.
func (w *Writer) Reset() { w.buf = w.buf[:0] }

// WriteBool writes a 0 or 1 byte.
func (w *Writer) WriteBool(v bool) {
	if v {
		w.buf = append(w.buf, 1)
	} else {
		w.buf = append(w.buf, 0)
	}
}

// WriteUint8 / WriteInt8 — single-byte primitives.
func (w *Writer) WriteUint8(v uint8) { w.buf = append(w.buf, v) }
func (w *Writer) WriteInt8(v int8)   { w.buf = append(w.buf, byte(v)) }

// WriteUint16 / WriteInt16 — little-endian 16-bit.
func (w *Writer) WriteUint16(v uint16) {
	w.buf = binary.LittleEndian.AppendUint16(w.buf, v)
}
func (w *Writer) WriteInt16(v int16) { w.WriteUint16(uint16(v)) }

// WriteUint32 / WriteInt32 — little-endian 32-bit.
func (w *Writer) WriteUint32(v uint32) {
	w.buf = binary.LittleEndian.AppendUint32(w.buf, v)
}
func (w *Writer) WriteInt32(v int32) { w.WriteUint32(uint32(v)) }

// WriteUint64 / WriteInt64 — little-endian 64-bit.
func (w *Writer) WriteUint64(v uint64) {
	w.buf = binary.LittleEndian.AppendUint64(w.buf, v)
}
func (w *Writer) WriteInt64(v int64) { w.WriteUint64(uint64(v)) }

// WriteVarUint writes an LEB128 unsigned_int.
func (w *Writer) WriteVarUint(v uint32) { w.buf = AppendVarUint(w.buf, v) }

// WriteVarInt writes a ZigZag+LEB128 signed_int.
func (w *Writer) WriteVarInt(v int32) { w.buf = AppendVarInt(w.buf, v) }

// WriteRaw appends src verbatim, with no length prefix. Matches
// fc::array<T, N> and similar fixed-layout types.
func (w *Writer) WriteRaw(src []byte) { w.buf = append(w.buf, src...) }

// WriteBytes writes a length-prefixed byte slice (`std::vector<char>`).
func (w *Writer) WriteBytes(src []byte) {
	w.buf = AppendVarUint(w.buf, uint32(len(src)))
	w.buf = append(w.buf, src...)
}

// WriteString writes a length-prefixed UTF-8 string (`fc::string`).
func (w *Writer) WriteString(s string) {
	w.buf = AppendVarUint(w.buf, uint32(len(s)))
	w.buf = append(w.buf, s...)
}

// WriteTimePointSec writes a 4-byte uint32 LE seconds-since-epoch.
func (w *Writer) WriteTimePointSec(unixSec uint32) { w.WriteUint32(unixSec) }

// WriteTimePoint writes an 8-byte uint64 LE microseconds-since-epoch.
func (w *Writer) WriteTimePoint(unixMicro uint64) { w.WriteUint64(unixMicro) }
