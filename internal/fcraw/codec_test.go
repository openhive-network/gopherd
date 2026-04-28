package fcraw

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

func TestPrimitiveRoundTrip(t *testing.T) {
	w := NewWriter()
	w.WriteBool(true)
	w.WriteBool(false)
	w.WriteUint8(0xab)
	w.WriteInt8(-5)
	w.WriteUint16(0x1234)
	w.WriteInt16(-12345)
	w.WriteUint32(0xdeadbeef)
	w.WriteInt32(math.MinInt32)
	w.WriteUint64(0xfeedfacecafebeef)
	w.WriteInt64(math.MaxInt64)
	w.WriteVarUint(300)
	w.WriteVarInt(-7)
	w.WriteString("hive")
	w.WriteBytes([]byte{0x01, 0x02, 0x03})
	w.WriteRaw([]byte{0xff, 0x00, 0xff})

	r := NewReader(w.Bytes())

	if v, err := r.ReadBool(); err != nil || !v {
		t.Fatalf("ReadBool true: %v %v", v, err)
	}
	if v, err := r.ReadBool(); err != nil || v {
		t.Fatalf("ReadBool false: %v %v", v, err)
	}
	if v, err := r.ReadUint8(); err != nil || v != 0xab {
		t.Fatalf("ReadUint8: %v %v", v, err)
	}
	if v, err := r.ReadInt8(); err != nil || v != -5 {
		t.Fatalf("ReadInt8: %v %v", v, err)
	}
	if v, err := r.ReadUint16(); err != nil || v != 0x1234 {
		t.Fatalf("ReadUint16: %v %v", v, err)
	}
	if v, err := r.ReadInt16(); err != nil || v != -12345 {
		t.Fatalf("ReadInt16: %v %v", v, err)
	}
	if v, err := r.ReadUint32(); err != nil || v != 0xdeadbeef {
		t.Fatalf("ReadUint32: %v %v", v, err)
	}
	if v, err := r.ReadInt32(); err != nil || v != math.MinInt32 {
		t.Fatalf("ReadInt32: %v %v", v, err)
	}
	if v, err := r.ReadUint64(); err != nil || v != 0xfeedfacecafebeef {
		t.Fatalf("ReadUint64: %v %v", v, err)
	}
	if v, err := r.ReadInt64(); err != nil || v != math.MaxInt64 {
		t.Fatalf("ReadInt64: %v %v", v, err)
	}
	if v, err := r.ReadVarUint(); err != nil || v != 300 {
		t.Fatalf("ReadVarUint: %v %v", v, err)
	}
	if v, err := r.ReadVarInt(); err != nil || v != -7 {
		t.Fatalf("ReadVarInt: %v %v", v, err)
	}
	if v, err := r.ReadString(); err != nil || v != "hive" {
		t.Fatalf("ReadString: %q %v", v, err)
	}
	if v, err := r.ReadBytes(); err != nil || !bytes.Equal(v, []byte{1, 2, 3}) {
		t.Fatalf("ReadBytes: %v %v", v, err)
	}
	got, err := r.ReadRaw(3)
	if err != nil || !bytes.Equal(got, []byte{0xff, 0x00, 0xff}) {
		t.Fatalf("ReadRaw: %v %v", got, err)
	}
	if !r.AtEnd() {
		t.Fatalf("expected reader at end, %d bytes remain", r.Remaining())
	}
}

func TestLittleEndianWireFormat(t *testing.T) {
	// uint32 0xdeadbeef must serialize as ef be ad de (LE).
	w := NewWriter()
	w.WriteUint32(0xdeadbeef)
	want := []byte{0xef, 0xbe, 0xad, 0xde}
	if !bytes.Equal(w.Bytes(), want) {
		t.Fatalf("uint32 LE: got %x, want %x", w.Bytes(), want)
	}
}

func TestBoolRejectsInvalidByte(t *testing.T) {
	r := NewReader([]byte{0x02})
	if _, err := r.ReadBool(); err == nil {
		t.Fatal("expected error on bool byte 0x02")
	}
}

func TestReadEOF(t *testing.T) {
	r := NewReader([]byte{0x01, 0x02})
	if _, err := r.ReadUint32(); !errors.Is(err, ErrUnexpectedEOF) {
		t.Fatalf("ReadUint32 on 2-byte input: got %v, want EOF", err)
	}
}

func TestReadStringRejectsOverlongLength(t *testing.T) {
	// Length 0x80 = 128 with only 4 actual bytes following.
	src := append([]byte{0x80, 0x01}, []byte("aaaa")...) // says 128 bytes follow
	r := NewReader(src)
	if _, err := r.ReadString(); err == nil {
		t.Fatal("expected error on overlong string length")
	}
}

func TestAliasReturnsZeroCopy(t *testing.T) {
	src := []byte{0x03, 'a', 'b', 'c'} // varint(3) + "abc"
	r := NewReader(src)
	got, err := r.ReadBytesAlias()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abc" {
		t.Fatalf("got %q, want abc", got)
	}
	// Mutating the source must be reflected in the alias (proves aliasing).
	src[1] = 'X'
	if string(got) != "Xbc" {
		t.Fatalf("alias did not reflect source mutation: got %q", got)
	}
}

func TestWriterReset(t *testing.T) {
	w := NewWriter()
	w.WriteString("first")
	w.Reset()
	w.WriteString("second")
	r := NewReader(w.Bytes())
	if v, err := r.ReadString(); err != nil || v != "second" {
		t.Fatalf("after Reset, got %q %v, want \"second\"", v, err)
	}
	if !r.AtEnd() {
		t.Fatal("reader not at end after Reset round-trip")
	}
}

func TestEmptyStringEncoding(t *testing.T) {
	// Empty string must be a single 0x00 length-prefix byte (no payload).
	w := NewWriter()
	w.WriteString("")
	if !bytes.Equal(w.Bytes(), []byte{0x00}) {
		t.Fatalf("empty string: got %x, want 00", w.Bytes())
	}
}

func TestTimePointSec(t *testing.T) {
	// Mainnet genesis timestamp 1458835200 = 0x56F40F00.
	// Little-endian on the wire: 00 0f f4 56.
	w := NewWriter()
	w.WriteTimePointSec(1458835200)
	want := []byte{0x00, 0x0f, 0xf4, 0x56}
	if !bytes.Equal(w.Bytes(), want) {
		t.Fatalf("time_point_sec: got %x, want %x", w.Bytes(), want)
	}
	r := NewReader(w.Bytes())
	if v, err := r.ReadTimePointSec(); err != nil || v != 1458835200 {
		t.Fatalf("ReadTimePointSec: %v %v", v, err)
	}
}
