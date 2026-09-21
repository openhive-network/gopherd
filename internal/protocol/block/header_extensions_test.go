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
