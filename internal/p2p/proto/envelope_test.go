package proto

import (
	"bytes"
	"io"
	"net/netip"
	"testing"

	"hived-go/internal/fcraw"
)

func TestPaddedLen(t *testing.T) {
	cases := []struct {
		body, want int
	}{
		{0, 16},  // header alone is 8, pads to 16
		{1, 16},  // 8+1 = 9 → 16
		{8, 16},  // 8+8 = 16 → 16
		{9, 32},  // 8+9 = 17 → 32
		{24, 32}, // 8+24 = 32 → 32
		{25, 48}, // 8+25 = 33 → 48
	}
	for _, c := range cases {
		if got := PaddedLen(c.body); got != c.want {
			t.Errorf("PaddedLen(%d) = %d, want %d", c.body, got, c.want)
		}
	}
}

func TestEncodeDecodeMessageRoundtrip(t *testing.T) {
	body := []byte("the quick brown fox jumps over the lazy dog")
	enc, err := EncodeMessage(HelloMessageType, body)
	if err != nil {
		t.Fatalf("EncodeMessage: %v", err)
	}
	if len(enc) != PaddedLen(len(body)) {
		t.Fatalf("envelope length: got %d, want %d", len(enc), PaddedLen(len(body)))
	}
	if got := uint32(enc[0]) | uint32(enc[1])<<8 | uint32(enc[2])<<16 | uint32(enc[3])<<24; got != uint32(len(body)) {
		t.Fatalf("size word: got %d, want %d", got, len(body))
	}
	if got := uint32(enc[4]) | uint32(enc[5])<<8 | uint32(enc[6])<<16 | uint32(enc[7])<<24; got != uint32(HelloMessageType) {
		t.Fatalf("type word: got %d, want %d", got, HelloMessageType)
	}
	for i := HeaderSize + len(body); i < len(enc); i++ {
		if enc[i] != 0 {
			t.Fatalf("padding byte %d not zero: %02x", i, enc[i])
		}
	}

	r := bytes.NewReader(enc)
	gotType, gotBody, err := DecodeMessage(r)
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if gotType != HelloMessageType {
		t.Fatalf("type: got %d, want %d", gotType, HelloMessageType)
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("body: got %x, want %x", gotBody, body)
	}
}

func TestEncodeMessageEmptyBody(t *testing.T) {
	enc, err := EncodeMessage(ConnectionAcceptedMessageType, nil)
	if err != nil {
		t.Fatalf("EncodeMessage: %v", err)
	}
	if len(enc) != HeaderSize+8 {
		t.Fatalf("envelope length: got %d, want %d", len(enc), HeaderSize+8)
	}
	want := []byte{
		0x00, 0x00, 0x00, 0x00, // size = 0
		0x8f, 0x13, 0x00, 0x00, // type = 5007
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // padding
	}
	if !bytes.Equal(enc, want) {
		t.Fatalf("empty connection_accepted envelope:\n got %x\nwant %x", enc, want)
	}
	gotType, gotBody, err := DecodeMessage(bytes.NewReader(enc))
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if gotType != ConnectionAcceptedMessageType || len(gotBody) != 0 {
		t.Fatalf("DecodeMessage: type=%d body=%x", gotType, gotBody)
	}
}

func TestDecodeMessageEOFOnCleanClose(t *testing.T) {
	if _, _, err := DecodeMessage(bytes.NewReader(nil)); err != io.EOF {
		t.Fatalf("DecodeMessage on closed stream: got %v, want io.EOF", err)
	}
}

func TestDecodeMessageShortHeader(t *testing.T) {
	if _, _, err := DecodeMessage(bytes.NewReader([]byte{1, 2, 3})); err != io.ErrUnexpectedEOF {
		t.Fatalf("DecodeMessage with 3-byte input: got %v, want ErrUnexpectedEOF", err)
	}
}

func TestEncodeMessageRejectsTooLarge(t *testing.T) {
	body := make([]byte, MaxMessageSize+1)
	if _, err := EncodeMessage(BlockMessageType, body); err == nil {
		t.Fatal("expected ErrMessageTooLarge")
	}
}

// IP tests live with the envelope-layer tests since both are pure
// supporting primitives used by the message-level tests below.

func TestAddressRoundtripV4(t *testing.T) {
	addr := netip.MustParseAddr("203.0.113.42")
	enc := AppendAddress(nil, addr)
	want := []byte{0x04, 203, 0, 113, 42}
	if !bytes.Equal(enc, want) {
		t.Fatalf("v4 wire: got %x, want %x", enc, want)
	}
	r := fcraw.NewReader(enc)
	got, err := ReadAddress(r)
	if err != nil {
		t.Fatalf("ReadAddress: %v", err)
	}
	if got != addr {
		t.Fatalf("addr: got %v, want %v", got, addr)
	}
}

func TestAddressRoundtripV6(t *testing.T) {
	addr := netip.MustParseAddr("2001:db8::1")
	enc := AppendAddress(nil, addr)
	if enc[0] != 0x06 || len(enc) != 17 {
		t.Fatalf("v6 wire prefix wrong: %x", enc)
	}
	r := fcraw.NewReader(enc)
	got, err := ReadAddress(r)
	if err != nil {
		t.Fatalf("ReadAddress: %v", err)
	}
	if got != addr {
		t.Fatalf("addr: got %v, want %v", got, addr)
	}
}

func TestEndpointRoundtrip(t *testing.T) {
	ep := MakeEndpoint(netip.MustParseAddr("198.51.100.7"), 2001)
	enc := AppendEndpoint(nil, ep)
	r := fcraw.NewReader(enc)
	got, err := ReadEndpoint(r)
	if err != nil {
		t.Fatalf("ReadEndpoint: %v", err)
	}
	if got.Addr.Addr() != ep.Addr.Addr() || got.Addr.Port() != ep.Addr.Port() {
		t.Fatalf("endpoint: got %v, want %v", got, ep)
	}
}

func TestLegacyAddressRoundtrip(t *testing.T) {
	addr := netip.MustParseAddr("10.20.30.40")
	enc, err := AppendLegacyAddress(nil, addr)
	if err != nil {
		t.Fatalf("AppendLegacyAddress: %v", err)
	}
	want := []byte{10, 20, 30, 40}
	if !bytes.Equal(enc, want) {
		t.Fatalf("legacy v4 wire: got %x, want %x", enc, want)
	}
	r := fcraw.NewReader(enc)
	got, err := ReadLegacyAddress(r)
	if err != nil {
		t.Fatalf("ReadLegacyAddress: %v", err)
	}
	if got != addr {
		t.Fatalf("addr: got %v, want %v", got, addr)
	}
}

func TestLegacyAddressRejectsV6(t *testing.T) {
	addr := netip.MustParseAddr("::1")
	if _, err := AppendLegacyAddress(nil, addr); err != ErrNotIPv4 {
		t.Fatalf("AppendLegacyAddress(v6): got %v, want ErrNotIPv4", err)
	}
}

func TestVariantRoundtripScalars(t *testing.T) {
	cases := []Variant{
		NewVariantNull(),
		NewVariantBool(true),
		NewVariantBool(false),
		NewVariantInt64(-12345),
		NewVariantUint64(0xfeedfacecafebeef),
		NewVariantDouble(3.14159265358979),
		NewVariantString("the quick brown fox"),
		NewVariantBlob([]byte{0xde, 0xad, 0xbe, 0xef}),
	}
	for _, v := range cases {
		enc := AppendVariant(nil, v)
		got, err := ReadVariant(fcraw.NewReader(enc))
		if err != nil {
			t.Fatalf("variant tag %d: ReadVariant: %v", v.Type, err)
		}
		if !v.Equal(got) {
			t.Fatalf("variant tag %d: roundtrip mismatch:\n got %+v\nwant %+v", v.Type, got, v)
		}
	}
}

func TestVariantArrayAndObject(t *testing.T) {
	o := NewVarObject()
	o.Set("platform", NewVariantString("linux"))
	o.Set("bitness", NewVariantUint64(64))
	o.Set("hardforks", NewVariantArray([]Variant{
		NewVariantUint64(1),
		NewVariantUint64(2),
		NewVariantUint64(20),
	}))
	v := NewVariantObject(o)
	enc := AppendVariant(nil, v)
	got, err := ReadVariant(fcraw.NewReader(enc))
	if err != nil {
		t.Fatalf("ReadVariant: %v", err)
	}
	if !v.Equal(got) {
		t.Fatalf("variant_object roundtrip mismatch")
	}
}

func TestVariantObjectInsertionOrder(t *testing.T) {
	o := NewVarObject()
	o.Set("z", NewVariantUint64(1))
	o.Set("a", NewVariantUint64(2))
	o.Set("m", NewVariantUint64(3))
	enc := AppendVariantObject(nil, o)
	got, err := ReadVariantObject(fcraw.NewReader(enc))
	if err != nil {
		t.Fatalf("ReadVariantObject: %v", err)
	}
	want := []string{"z", "a", "m"}
	gotKeys := got.Keys()
	if len(gotKeys) != len(want) {
		t.Fatalf("keys: got %v", gotKeys)
	}
	for i, k := range want {
		if gotKeys[i] != k {
			t.Fatalf("insertion order: got[%d]=%q, want %q (full %v)", i, gotKeys[i], k, gotKeys)
		}
	}
}
