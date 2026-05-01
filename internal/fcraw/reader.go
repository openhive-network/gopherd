package fcraw

import (
	"encoding/binary"
	"errors"
	"io"
)

var (
	// ErrUnexpectedEOF is returned when a Read* call runs past the end of the
	// input window.
	ErrUnexpectedEOF = io.ErrUnexpectedEOF

	errInvalidBool   = errors.New("fcraw: invalid bool byte (must be 0 or 1)")
	errStringTooLong = errors.New("fcraw: string/bytes length exceeds remaining input")
)

// Reader is a cursor over a fixed []byte window, decoding FC binary primitives
// in sequence. It is allocation-free for fixed-size primitives and supports
// zero-copy aliasing for variable-length data via ReadBytesAlias / ReadStringAlias.
//
// Reader is not safe for concurrent use.
type Reader struct {
	src []byte
	pos int
}

// NewReader wraps src for reading. The Reader holds src by reference; the
// caller must keep src alive for the lifetime of any aliased slices returned
// by ReadBytesAlias / ReadStringAlias.
func NewReader(src []byte) *Reader { return &Reader{src: src} }

// Reset rewinds the Reader to a new source.
func (r *Reader) Reset(src []byte) { r.src = src; r.pos = 0 }

// Pos returns the current byte offset.
func (r *Reader) Pos() int { return r.pos }

// Remaining returns the number of unread bytes.
func (r *Reader) Remaining() int { return len(r.src) - r.pos }

// AtEnd reports whether the cursor has consumed all input.
func (r *Reader) AtEnd() bool { return r.pos >= len(r.src) }

// SourceRange returns the byte slice src[start:end], aliased into the
// underlying source. The cursor is not affected. Callers must not mutate
// the returned slice and must keep the source alive while it is in use.
// Useful for capturing the raw byte range a parser consumed (e.g. for
// hashing or round-trip preservation).
func (r *Reader) SourceRange(start, end int) []byte {
	if start < 0 || end > len(r.src) || end < start {
		return nil
	}
	return r.src[start:end:end]
}

// ensure checks that at least n bytes remain.
func (r *Reader) ensure(n int) error {
	if r.pos+n > len(r.src) {
		return ErrUnexpectedEOF
	}
	return nil
}

// ReadByte reads a single byte.
func (r *Reader) ReadByte() (byte, error) {
	if err := r.ensure(1); err != nil {
		return 0, err
	}
	b := r.src[r.pos]
	r.pos++
	return b, nil
}

// ReadBool reads a single byte and asserts it is 0 or 1.
func (r *Reader) ReadBool() (bool, error) {
	b, err := r.ReadByte()
	if err != nil {
		return false, err
	}
	if b > 1 {
		return false, errInvalidBool
	}
	return b == 1, nil
}

// ReadUint8 / ReadInt8.
func (r *Reader) ReadUint8() (uint8, error) { return r.ReadByte() }
func (r *Reader) ReadInt8() (int8, error)   { b, err := r.ReadByte(); return int8(b), err }

// for symmetry with ReadUint8 in places where we want explicit naming.
var _ = (*Reader).ReadUint8

// ReadUint16 / ReadInt16 — little-endian.
func (r *Reader) ReadUint16() (uint16, error) {
	if err := r.ensure(2); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint16(r.src[r.pos:])
	r.pos += 2
	return v, nil
}
func (r *Reader) ReadInt16() (int16, error) { v, err := r.ReadUint16(); return int16(v), err }

// ReadUint32 / ReadInt32 — little-endian.
func (r *Reader) ReadUint32() (uint32, error) {
	if err := r.ensure(4); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint32(r.src[r.pos:])
	r.pos += 4
	return v, nil
}
func (r *Reader) ReadInt32() (int32, error) { v, err := r.ReadUint32(); return int32(v), err }

// ReadUint64 / ReadInt64 — little-endian.
func (r *Reader) ReadUint64() (uint64, error) {
	if err := r.ensure(8); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint64(r.src[r.pos:])
	r.pos += 8
	return v, nil
}
func (r *Reader) ReadInt64() (int64, error) { v, err := r.ReadUint64(); return int64(v), err }

// ReadVarUint reads an LEB128 unsigned_int.
func (r *Reader) ReadVarUint() (uint32, error) {
	v, n, err := ReadVarUint(r.src, r.pos)
	if err != nil {
		return 0, err
	}
	r.pos += n
	return v, nil
}

// ReadVarInt reads a ZigZag+LEB128 signed_int.
func (r *Reader) ReadVarInt() (int32, error) {
	v, n, err := ReadVarInt(r.src, r.pos)
	if err != nil {
		return 0, err
	}
	r.pos += n
	return v, nil
}

// ReadRaw reads exactly n bytes. The returned slice is a fresh allocation.
func (r *Reader) ReadRaw(n int) ([]byte, error) {
	if err := r.ensure(n); err != nil {
		return nil, err
	}
	out := make([]byte, n)
	copy(out, r.src[r.pos:r.pos+n])
	r.pos += n
	return out, nil
}

// ReadRawAlias reads n bytes and returns a slice aliased into the source
// buffer. Callers must not mutate the returned slice and must keep the source
// alive while it is in use. Use this in zero-allocation hot paths.
func (r *Reader) ReadRawAlias(n int) ([]byte, error) {
	if err := r.ensure(n); err != nil {
		return nil, err
	}
	out := r.src[r.pos : r.pos+n : r.pos+n]
	r.pos += n
	return out, nil
}

// ReadInto reads exactly len(dst) bytes into dst.
func (r *Reader) ReadInto(dst []byte) error {
	if err := r.ensure(len(dst)); err != nil {
		return err
	}
	copy(dst, r.src[r.pos:r.pos+len(dst)])
	r.pos += len(dst)
	return nil
}

// readLengthPrefix reads a varint length and bounds-checks it against the
// remaining input.
func (r *Reader) readLengthPrefix() (int, error) {
	n, err := r.ReadVarUint()
	if err != nil {
		return 0, err
	}
	if int(n) > r.Remaining() {
		return 0, errStringTooLong
	}
	return int(n), nil
}

// ReadBytes reads a length-prefixed byte slice and returns a fresh allocation.
func (r *Reader) ReadBytes() ([]byte, error) {
	n, err := r.readLengthPrefix()
	if err != nil {
		return nil, err
	}
	out := make([]byte, n)
	copy(out, r.src[r.pos:r.pos+n])
	r.pos += n
	return out, nil
}

// ReadBytesAlias reads a length-prefixed byte slice and returns a slice
// aliased into the source buffer. See ReadRawAlias for safety constraints.
func (r *Reader) ReadBytesAlias() ([]byte, error) {
	n, err := r.readLengthPrefix()
	if err != nil {
		return nil, err
	}
	out := r.src[r.pos : r.pos+n : r.pos+n]
	r.pos += n
	return out, nil
}

// ReadString reads a length-prefixed UTF-8 string.
func (r *Reader) ReadString() (string, error) {
	n, err := r.readLengthPrefix()
	if err != nil {
		return "", err
	}
	src := r.src[r.pos : r.pos+n]
	r.pos += n
	return string(src), nil
}

// ReadTimePointSec reads a 4-byte uint32 LE.
func (r *Reader) ReadTimePointSec() (uint32, error) { return r.ReadUint32() }

// ReadTimePoint reads an 8-byte uint64 LE microseconds.
func (r *Reader) ReadTimePoint() (uint64, error) { return r.ReadUint64() }
