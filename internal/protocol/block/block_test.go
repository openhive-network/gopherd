package block

import (
	"bytes"
	"testing"
)

func TestNumFromID(t *testing.T) {
	var id ID
	id[0], id[1], id[2], id[3] = 0x00, 0x01, 0x86, 0xa0 // 100000 BE
	if got := id.Num(); got != 100000 {
		t.Fatalf("ID.Num: got %d, want 100000", got)
	}
}

func TestConstructIDEmbedsBlockNum(t *testing.T) {
	bytes := []byte("arbitrary bytes that don't matter for this test")
	id := ConstructID(bytes, 12345)
	if id.Num() != 12345 {
		t.Fatalf("ConstructID embedded block_num=%d, want 12345", id.Num())
	}
}

func TestConstructIDIsDeterministic(t *testing.T) {
	a := ConstructID([]byte{1, 2, 3}, 7)
	b := ConstructID([]byte{1, 2, 3}, 7)
	if a != b {
		t.Fatal("ConstructID must be deterministic for the same input")
	}
}

func TestSignedHeaderRoundTrip(t *testing.T) {
	h := SignedHeader{
		Header: Header{
			Previous:              ID{0, 0, 0, 1, 0xaa, 0xbb, 0xcc, 0xdd},
			Timestamp:             1458835200,
			Witness:               "initminer",
			TransactionMerkleRoot: [IDSize]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20},
			Extensions:            []byte{0x00},
		},
	}
	for i := range h.WitnessSignature {
		h.WitnessSignature[i] = byte(i)
	}

	wire := MarshalSignedHeader(nil, &h)
	got, n, err := UnmarshalSignedHeader(wire)
	if err != nil {
		t.Fatalf("UnmarshalSignedHeader: %v", err)
	}
	if n != len(wire) {
		t.Fatalf("expected to consume %d bytes, consumed %d", len(wire), n)
	}
	if got.Previous != h.Previous {
		t.Errorf("Previous mismatch")
	}
	if got.Timestamp != h.Timestamp {
		t.Errorf("Timestamp mismatch: got %d, want %d", got.Timestamp, h.Timestamp)
	}
	if got.Witness != h.Witness {
		t.Errorf("Witness mismatch: got %q, want %q", got.Witness, h.Witness)
	}
	if got.TransactionMerkleRoot != h.TransactionMerkleRoot {
		t.Errorf("MerkleRoot mismatch")
	}
	if !bytes.Equal(got.Extensions, h.Extensions) {
		t.Errorf("Extensions mismatch: got %x, want %x", got.Extensions, h.Extensions)
	}
	if got.WitnessSignature != h.WitnessSignature {
		t.Errorf("WitnessSignature mismatch")
	}

	// Re-marshal and confirm bit-exact equivalence.
	wire2 := MarshalSignedHeader(nil, &got)
	if !bytes.Equal(wire, wire2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", wire2, wire)
	}
}

func TestUnmarshalParsesVersionExtension(t *testing.T) {
	// Build a header carrying one block_header_extensions entry of tag 1
	// (version): varint(1) count + varint(1) tag + 4 bytes v_num.
	versionBytes := []byte{0x01, 0x01, 0xab, 0xcd, 0x00, 0x00}
	h := SignedHeader{
		Header: Header{
			Witness:    "alice",
			Extensions: versionBytes,
		},
	}
	wire := MarshalSignedHeader(nil, &h)
	got, n, err := UnmarshalSignedHeader(wire)
	if err != nil {
		t.Fatalf("UnmarshalSignedHeader with version ext: %v", err)
	}
	if n != len(wire) {
		t.Fatalf("consumed %d, want %d", n, len(wire))
	}
	if !bytesEq(got.Extensions, versionBytes) {
		t.Fatalf("extensions mismatch:\n got %x\nwant %x", got.Extensions, versionBytes)
	}
}

func TestUnmarshalParsesHardforkVoteExtension(t *testing.T) {
	// tag 2 + 8 bytes body (hardfork_version + time_point_sec).
	body := []byte{
		0x01, 0x02, // varint(1) count, varint(2) tag
		0x00, 0x00, 0x14, 0x00, // v_num
		0x80, 0xab, 0xcd, 0x5e, // time_point_sec
	}
	h := SignedHeader{
		Header: Header{
			Witness:    "alice",
			Extensions: body,
		},
	}
	wire := MarshalSignedHeader(nil, &h)
	got, _, err := UnmarshalSignedHeader(wire)
	if err != nil {
		t.Fatalf("UnmarshalSignedHeader with hardfork_version_vote: %v", err)
	}
	if !bytesEq(got.Extensions, body) {
		t.Fatalf("extensions mismatch:\n got %x\nwant %x", got.Extensions, body)
	}
}

func TestUnmarshalRejectsUnknownExtensionTag(t *testing.T) {
	body := []byte{0x01, 0x05} // count=1, tag=5 (unknown)
	h := SignedHeader{
		Header: Header{
			Witness:    "alice",
			Extensions: body,
		},
	}
	wire := MarshalSignedHeader(nil, &h)
	if _, _, err := UnmarshalSignedHeader(wire); err == nil {
		t.Fatal("expected error for unknown extension tag")
	}
}

func bytesEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
