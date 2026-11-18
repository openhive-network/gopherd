package block

import (
	"bytes"
	"testing"
)

// TestAppendHeaderExtensionsRoundTrip pairs the encoder with the parser that
// has always been here, across every variant.
func TestAppendHeaderExtensionsRoundTrip(t *testing.T) {
	in := []HeaderExtension{
		{Tag: HeaderExtVersion, Version: 0x011C0007},
		{Tag: HeaderExtHardforkVersionVote, HardforkVersion: 0x011D0000, HardforkVersionTime: 4102444800},
	}
	got, err := ParseHeaderExtensions(AppendHeaderExtensions(nil, in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != len(in) {
		t.Fatalf("got %d entries, want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("entry %d: got %+v, want %+v", i, got[i], in[i])
		}
	}
}

// TestAppendHeaderExtensionsEmpty — an extensions-less block carries a single
// zero byte, which is what every block on the chain has. Getting this wrong
// would change the signing digest of every block we produce.
func TestAppendHeaderExtensionsEmpty(t *testing.T) {
	if got := AppendHeaderExtensions(nil, nil); !bytes.Equal(got, []byte{0}) {
		t.Errorf("got %v, want [0]", got)
	}
	if got := AppendHeaderExtensions(nil, []HeaderExtension{}); !bytes.Equal(got, []byte{0}) {
		t.Errorf("empty slice: got %v, want [0]", got)
	}
}

// TestAppendHeaderExtensionsIsASet — hived's extensions member is a
// flat_set<block_header_extensions>: sorted by variant tag, deduped. A header
// built in the other order has to serialize identically, or the block we sign
// differs byte-for-byte from the one hived would sign for the same content.
func TestAppendHeaderExtensionsIsASet(t *testing.T) {
	version := HeaderExtension{Tag: HeaderExtVersion, Version: 0x011C0007}
	vote := HeaderExtension{Tag: HeaderExtHardforkVersionVote, HardforkVersion: 0x011D0000, HardforkVersionTime: 99}

	ordered := AppendHeaderExtensions(nil, []HeaderExtension{version, vote})
	reversed := AppendHeaderExtensions(nil, []HeaderExtension{vote, version})
	if !bytes.Equal(ordered, reversed) {
		t.Errorf("input order changed the encoding:\n  %v\n  %v", ordered, reversed)
	}

	withDupes := AppendHeaderExtensions(nil, []HeaderExtension{vote, version, version, vote})
	if !bytes.Equal(ordered, withDupes) {
		t.Errorf("duplicates survived:\n  %v\n  %v", ordered, withDupes)
	}

	// Exact bytes: count=2, then tag 1 + v_num LE, then tag 2 + v_num + time.
	want := []byte{
		0x02,
		0x01, 0x07, 0x00, 0x1C, 0x01,
		0x02, 0x00, 0x00, 0x1D, 0x01, 0x63, 0x00, 0x00, 0x00,
	}
	if !bytes.Equal(ordered, want) {
		t.Errorf("encoding:\n got %v\nwant %v", ordered, want)
	}
}

// TestAppendHeaderExtensionsVoidVariant — tag 0 has no body. It is never
// produced, but it is a legal variant and must not desynchronise the stream.
func TestAppendHeaderExtensionsVoidVariant(t *testing.T) {
	in := []HeaderExtension{{Tag: HeaderExtVoid}, {Tag: HeaderExtVersion, Version: 7}}
	got, err := ParseHeaderExtensions(AppendHeaderExtensions(nil, in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 || got[0].Tag != HeaderExtVoid || got[1].Version != 7 {
		t.Errorf("got %+v", got)
	}
}

// TestStateStampExtensionWire pins tag 3 = state_stamp { checksum_type
// merkle_root } (hived 18045a0ee): a varint tag then the 20 raw bytes of the
// ripemd160, and the signed-header parser must step over it.
func TestStateStampExtensionWire(t *testing.T) {
	var root [20]byte
	for i := range root {
		root[i] = byte(0xA0 + i)
	}
	stamp := HeaderExtension{Tag: HeaderExtStateStamp, StateMerkleRoot: root}
	version := HeaderExtension{Tag: HeaderExtVersion, Version: 0x011D0000}
	raw := AppendHeaderExtensions(nil, []HeaderExtension{stamp, version})
	want := append([]byte{0x02, 0x01, 0x00, 0x00, 0x1D, 0x01, 0x03}, root[:]...)
	if !bytes.Equal(raw, want) {
		t.Fatalf("encoding:\n got %x\nwant %x", raw, want)
	}
	got, err := ParseHeaderExtensions(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != version || got[1] != stamp {
		t.Fatalf("parsed %+v", got)
	}

	h := SignedHeader{Header: Header{Witness: "initminer", Extensions: raw}}
	h.WitnessSignature[0] = 0x1f
	enc := MarshalSignedHeader(nil, &h)
	dec, n, err := UnmarshalSignedHeader(enc)
	if err != nil {
		t.Fatalf("UnmarshalSignedHeader: %v", err)
	}
	if n != len(enc) || !bytes.Equal(dec.Extensions, raw) || dec.WitnessSignature != h.WitnessSignature {
		t.Fatalf("signed header did not round-trip: n=%d/%d ext=%x", n, len(enc), dec.Extensions)
	}

	// A truncated root is an error, not a short read.
	if _, err := ParseHeaderExtensions(raw[:len(raw)-1]); err == nil {
		t.Error("truncated state_stamp parsed")
	}
	// Tag 4 is still unknown.
	if _, err := ParseHeaderExtensions([]byte{0x01, 0x04}); err == nil {
		t.Error("tag 4 parsed")
	}
}

// TestParseHeaderExtensionsIsHivedsFlatSet — fc unpacks a flat_set by
// insert(), and static_variant compares by tag alone, so hived holds the wire
// list sorted by tag with every repeated tag dropped, the first one kept. Two
// state stamps in one block are therefore one (the first) to hived, which is
// why its duplicate-stamp assertion is unreachable.
func TestParseHeaderExtensionsIsHivedsFlatSet(t *testing.T) {
	first := [20]byte{1}
	second := [20]byte{2}
	raw := []byte{0x04}
	raw = append(append(raw, 0x03), first[:]...)
	raw = append(raw, 0x01, 0x07, 0x00, 0x00, 0x00)
	raw = append(append(raw, 0x03), second[:]...)
	raw = append(raw, 0x01, 0x08, 0x00, 0x00, 0x00)
	got, err := ParseHeaderExtensions(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []HeaderExtension{
		{Tag: HeaderExtVersion, Version: 7},
		{Tag: HeaderExtStateStamp, StateMerkleRoot: first},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	// The encoder is the same set: re-encoding keeps the first of each tag.
	if enc := AppendHeaderExtensions(nil, []HeaderExtension{
		{Tag: HeaderExtStateStamp, StateMerkleRoot: first},
		{Tag: HeaderExtStateStamp, StateMerkleRoot: second},
	}); !bytes.Equal(enc, append([]byte{0x01, 0x03}, first[:]...)) {
		t.Errorf("encoder kept %x", enc)
	}
}
