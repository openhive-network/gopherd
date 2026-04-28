package fcraw

import (
	"bytes"
	"math"
	"testing"
)

// Vectors derived from fc::raw::pack(unsigned_int) by walking the algorithm
// by hand. Re-verifying these against an actual hived run is part of the
// testvec capture step, not the unit-test gate.
var varUintVectors = []struct {
	v    uint32
	want []byte
}{
	{0, []byte{0x00}},
	{1, []byte{0x01}},
	{0x7f, []byte{0x7f}},
	{0x80, []byte{0x80, 0x01}},
	{0x3fff, []byte{0xff, 0x7f}},
	{0x4000, []byte{0x80, 0x80, 0x01}},
	{0x1fffff, []byte{0xff, 0xff, 0x7f}},
	{0x200000, []byte{0x80, 0x80, 0x80, 0x01}},
	{0xfffffff, []byte{0xff, 0xff, 0xff, 0x7f}},
	{0x10000000, []byte{0x80, 0x80, 0x80, 0x80, 0x01}},
	{math.MaxUint32, []byte{0xff, 0xff, 0xff, 0xff, 0x0f}},
}

func TestAppendVarUint(t *testing.T) {
	for _, tc := range varUintVectors {
		got := AppendVarUint(nil, tc.v)
		if !bytes.Equal(got, tc.want) {
			t.Errorf("AppendVarUint(%#x) = %x, want %x", tc.v, got, tc.want)
		}
	}
}

func TestReadVarUint(t *testing.T) {
	for _, tc := range varUintVectors {
		v, n, err := ReadVarUint(tc.want, 0)
		if err != nil {
			t.Errorf("ReadVarUint(%x): unexpected error %v", tc.want, err)
			continue
		}
		if v != tc.v {
			t.Errorf("ReadVarUint(%x) = %#x, want %#x", tc.want, v, tc.v)
		}
		if n != len(tc.want) {
			t.Errorf("ReadVarUint(%x) consumed %d bytes, want %d", tc.want, n, len(tc.want))
		}
	}
}

func TestReadVarUintErrors(t *testing.T) {
	// Truncated: continuation bit set but no follow-up byte.
	if _, _, err := ReadVarUint([]byte{0x80}, 0); err == nil {
		t.Fatal("expected EOF on truncated varint")
	}
	// 6-byte varint (overflows uint32 cleanly past the 5-byte limit).
	overlong := []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x01}
	if _, _, err := ReadVarUint(overlong, 0); err == nil {
		t.Fatal("expected error on >5 byte varint")
	}
}

func TestVarUintRoundTrip(t *testing.T) {
	values := []uint32{0, 1, 127, 128, 16383, 16384, 0xdeadbeef, math.MaxUint32}
	for _, v := range values {
		enc := AppendVarUint(nil, v)
		dec, n, err := ReadVarUint(enc, 0)
		if err != nil {
			t.Fatalf("round-trip %#x: %v", v, err)
		}
		if dec != v {
			t.Fatalf("round-trip %#x decoded as %#x", v, dec)
		}
		if n != len(enc) {
			t.Fatalf("round-trip %#x length mismatch: got %d, encoded %d", v, n, len(enc))
		}
	}
}

func TestVarIntRoundTrip(t *testing.T) {
	values := []int32{0, 1, -1, 63, -63, 64, -64, 8191, -8191,
		math.MinInt32, math.MaxInt32}
	for _, v := range values {
		enc := AppendVarInt(nil, v)
		dec, n, err := ReadVarInt(enc, 0)
		if err != nil {
			t.Fatalf("round-trip %d: %v", v, err)
		}
		if dec != v {
			t.Fatalf("round-trip %d decoded as %d", v, dec)
		}
		if n != len(enc) {
			t.Fatalf("round-trip %d length mismatch", v)
		}
	}
}

// ZigZag-canonical: 0->0, -1->1, 1->2, -2->3, 2->4, ... so the byte pattern
// for small values must match these.
func TestVarIntZigzagCanonical(t *testing.T) {
	cases := []struct {
		v    int32
		want []byte
	}{
		{0, []byte{0x00}},
		{-1, []byte{0x01}},
		{1, []byte{0x02}},
		{-2, []byte{0x03}},
		{2, []byte{0x04}},
		{63, []byte{0x7e}},
		{-64, []byte{0x7f}},
		{64, []byte{0x80, 0x01}},
	}
	for _, tc := range cases {
		got := AppendVarInt(nil, tc.v)
		if !bytes.Equal(got, tc.want) {
			t.Errorf("AppendVarInt(%d) = %x, want %x", tc.v, got, tc.want)
		}
	}
}
