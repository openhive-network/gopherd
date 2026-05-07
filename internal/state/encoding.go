package state

import "encoding/binary"

// Order-preserving encoders for state index keys.
//
// State keys are NOT the same as wire/serialization bytes — they're chosen
// so that Pebble's lexicographic byte-ordering reproduces the application-
// level sort order. Big-endian for unsigned ints (so byte-wise sort = numeric
// sort), sign-bit flipped for signed ints, fixed-width for variable-length
// strings (NUL-padded so prefix sorts agree with comparison sorts).

// AppendUint32BE appends v as 4 big-endian bytes.
func AppendUint32BE(dst []byte, v uint32) []byte {
	return binary.BigEndian.AppendUint32(dst, v)
}

// AppendUint64BE appends v as 8 big-endian bytes.
func AppendUint64BE(dst []byte, v uint64) []byte {
	return binary.BigEndian.AppendUint64(dst, v)
}

// AppendInt32BE appends v with the sign bit flipped, then 4 big-endian
// bytes — produces a byte sequence whose lexicographic order matches the
// signed numeric order of v.
func AppendInt32BE(dst []byte, v int32) []byte {
	return binary.BigEndian.AppendUint32(dst, uint32(v)^(1<<31))
}

// AppendInt64BE appends v with the sign bit flipped, then 8 big-endian
// bytes.
func AppendInt64BE(dst []byte, v int64) []byte {
	return binary.BigEndian.AppendUint64(dst, uint64(v)^(1<<63))
}

// AccountNameKeyLen is the fixed width of an encoded account_name_type
// in state-key form. account_name_type is fixed_string<16> internally;
// in keys we store it NUL-padded to 16 bytes so prefix iteration works
// reliably. Names are restricted to ASCII (a-z, 0-9, '.', '-') so NUL
// is unambiguously a terminator.
const AccountNameKeyLen = 16

// AppendAccountName appends name as exactly AccountNameKeyLen bytes,
// truncating or NUL-padding as needed. Hot path: this is on every state
// lookup that takes an account_name_type, so the byte-by-byte loop is
// hand-rolled to avoid per-iteration append overhead — `var buf` lives
// on the stack, copy fills the prefix from name, the rest stays zero.
func AppendAccountName(dst []byte, name string) []byte {
	var buf [AccountNameKeyLen]byte
	copy(buf[:], name)
	return append(dst, buf[:]...)
}

// AppendTimePointSec appends a time_point_sec as 4 big-endian bytes.
func AppendTimePointSec(dst []byte, sec uint32) []byte {
	return binary.BigEndian.AppendUint32(dst, sec)
}

// AppendBool appends a single 0 or 1 byte.
func AppendBool(dst []byte, b bool) []byte {
	if b {
		return append(dst, 1)
	}
	return append(dst, 0)
}

// FlipForDescending XORs every byte of body with 0xff. Use this on a
// composite-key suffix that needs to sort descending (e.g. an index that
// orders by `-vesting_shares`).
func FlipForDescending(body []byte) []byte {
	out := make([]byte, len(body))
	for i, b := range body {
		out[i] = ^b
	}
	return out
}
