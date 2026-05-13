// Tests ported from hive/tests/unit/tests/serialization_tests.cpp boundary
// cases that exercise the FC raw reader's array-allocation guard. The C++ side
// uses fc::MAX_ARRAY_ALLOC_SIZE; Go bounds-checks each length prefix against
// the remaining input window via Reader.readLengthPrefix, so the practical
// effect — declared-length larger than the buffer fails fast — is the same.
package fcraw

import (
	"testing"
)

// TestUnpackArrayLengthExceedsInput ports
// serialization_tests.cpp::unpack_array_limit_test in spirit. The C++ test
// packs a varint length one greater than the actual data length and asserts
// fc::raw::unpack throws. Go's Reader.ReadBytes refuses to allocate beyond the
// source buffer's bounds: the moral equivalent.
func TestUnpackArrayLengthExceedsInput(t *testing.T) {
	// Encode varint(5) followed by only 3 bytes — reader must refuse.
	buf := AppendVarUint(nil, 5)
	buf = append(buf, 0x01, 0x02, 0x03)
	r := NewReader(buf)
	if _, err := r.ReadBytes(); err == nil {
		t.Fatal("expected error: declared length exceeds remaining input")
	}
}

// TestUnpackArrayLengthAtBoundary verifies the inverse: a length prefix that
// matches the exact remaining bytes succeeds. This pins behavior so that a
// future change to readLengthPrefix doesn't break a strict equality case.
func TestUnpackArrayLengthAtBoundary(t *testing.T) {
	const n = 1024
	payload := make([]byte, n)
	for i := range payload {
		payload[i] = byte(i)
	}
	buf := AppendVarUint(nil, uint32(n))
	buf = append(buf, payload...)
	r := NewReader(buf)
	out, err := r.ReadBytes()
	if err != nil {
		t.Fatalf("at-boundary read: %v", err)
	}
	if len(out) != n {
		t.Fatalf("read %d bytes, want %d", len(out), n)
	}
	if r.Remaining() != 0 {
		t.Fatalf("after read: %d bytes remain, want 0", r.Remaining())
	}
}

// TestUnpackClearReader ports serialization_tests.cpp::unpack_clear_test in
// spirit. The C++ test verifies that re-using an `unpacked_block` variable
// across two consecutive unpack calls produces a clean overwrite. Go has no
// equivalent "unpack into pre-existing object" API — every reader call
// produces a fresh value — but the analog is Reader.Reset, which lets the
// caller reuse one Reader across multiple buffers without leaking state.
// Verify Reset truly rewinds + adopts the new source.
func TestUnpackClearReader(t *testing.T) {
	first := AppendVarUint(nil, 7)
	first = append(first, []byte("abcdefg")...)

	second := AppendVarUint(nil, 3)
	second = append(second, []byte("xyz")...)

	r := NewReader(first)
	out, err := r.ReadBytes()
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if string(out) != "abcdefg" {
		t.Fatalf("first read = %q, want abcdefg", out)
	}
	// Reset and verify the Reader fully replaces its source — no stale
	// `pos` from the previous run.
	r.Reset(second)
	if r.Pos() != 0 {
		t.Fatalf("after Reset: pos=%d, want 0", r.Pos())
	}
	out2, err := r.ReadBytes()
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if string(out2) != "xyz" {
		t.Fatalf("second read = %q, want xyz", out2)
	}
	if r.Remaining() != 0 {
		t.Fatalf("after second read: %d bytes remain", r.Remaining())
	}
}

// TestUnpackRecursionLimit ports the spirit of
// serialization_tests.cpp::unpack_recursion_test. The C++ side packs 100k
// nested variant array tags and asserts the unpacker bails out. Go's reader
// has no recursion (it's a flat byte cursor), and the protocol-level decoders
// in op/ + block/ never call themselves recursively on user-controlled
// counts; the only practical recursion guard is varint-length-vs-remaining-
// input. Verify a very large varint inside a tiny buffer fails fast.
func TestUnpackRecursionLimit(t *testing.T) {
	// Encode a single varint of value MaxUint32, no payload.
	buf := AppendVarUint(nil, 0xFFFFFFFF)
	r := NewReader(buf)
	if _, err := r.ReadBytes(); err == nil {
		t.Fatal("expected error: declared length wildly exceeds buffer")
	}
}
