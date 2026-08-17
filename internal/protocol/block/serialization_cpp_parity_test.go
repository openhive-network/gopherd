// Tests ported from hive/tests/unit/tests/serialization_tests.cpp.
package block

import (
	"testing"

	"gopherd/internal/fcraw"
)

// TestMinBlockSize ports serialization_tests.cpp::min_block_size. The C++ test
// constructs an otherwise-empty signed_block with witness="aaa" (3 chars =
// HIVE_MIN_ACCOUNT_NAME_LENGTH) and checks `fc::raw::pack_size(b) ==
// HIVE_MIN_BLOCK_SIZE` (115).
//
// A signed_block on the wire is:
//
//	signed_block_header :
//	  previous              20 bytes
//	  timestamp             4 bytes  (uint32 LE)
//	  witness               1+N      (varint-prefixed string; N=3 here)
//	  transaction_merkle_root 20 bytes
//	  extensions            1 byte   (varint count = 0)
//	  witness_signature     65 bytes
//	transactions            1 byte   (varint count = 0)
//
// Total = 20+4+(1+3)+20+1+65+1 = 115.
//
// Go has no top-level signed_block type yet (block bytes are streamed through
// the p2p/sync compressed-block path), so we compute the size by serializing
// the SignedHeader plus a one-byte empty-trxs vector. This mirrors what
// fc::raw::pack_size would emit.
func TestMinBlockSize(t *testing.T) {
	h := SignedHeader{
		Header: Header{
			Witness:    "aaa", // HIVE_MIN_ACCOUNT_NAME_LENGTH
			Extensions: []byte{0x00},
		},
	}
	headerBytes := MarshalSignedHeader(nil, &h)
	// Append the empty transactions vector (varint count = 0 → single 0x00).
	full := append(headerBytes, 0x00)
	const HiveMinBlockSize = 115
	if got := len(full); got != HiveMinBlockSize {
		t.Fatalf("min signed_block size = %d, want %d", got, HiveMinBlockSize)
	}
}

// TestBlockHeaderVersionExtensionTypedDecode ports the spirit of
// serialization_tests.cpp::block_header_test. The C++ side checks that the
// legacy and hf26 JSON encodings of a block_header_extensions(version "1.2.3")
// hash identically. Go doesn't model variant-tag-as-typed-discriminant
// (encoding/json instead of fc::json), so we instead verify the binary path:
// build a header with a version=0x010203 extension and confirm
// ParseHeaderExtensions decodes the tag + v_num correctly.
func TestBlockHeaderVersionExtensionTypedDecode(t *testing.T) {
	// hardfork_version{1,2,3} packs as (1<<24)|(2<<16)|(3<<8) per the C++
	// `version` encoding: major<<24 | minor<<16 | rev (uint16).
	// For block_header_extensions::version the body is a single uint32 v_num.
	versionVNum := uint32(0x01020003) // major=1, minor=2, rev=3
	body := []byte{}
	body = fcraw.AppendVarUint(body, 1) // extensions count = 1
	body = fcraw.AppendVarUint(body, 1) // variant tag = 1 (version)
	body = append(body,
		byte(versionVNum),
		byte(versionVNum>>8),
		byte(versionVNum>>16),
		byte(versionVNum>>24),
	)
	exts, err := ParseHeaderExtensions(body)
	if err != nil {
		t.Fatalf("ParseHeaderExtensions: %v", err)
	}
	if len(exts) != 1 {
		t.Fatalf("extensions count = %d, want 1", len(exts))
	}
	if exts[0].Tag != HeaderExtVersion {
		t.Errorf("tag = %d, want HeaderExtVersion", exts[0].Tag)
	}
	if exts[0].Version != versionVNum {
		t.Errorf("v_num = %#x, want %#x", exts[0].Version, versionVNum)
	}
}

// TestBlockHeaderHardforkVoteExtensionTypedDecode mirrors the hardfork_version
// vote case alongside the version case above. v_num encodes 1.0.0, time fixed.
func TestBlockHeaderHardforkVoteExtensionTypedDecode(t *testing.T) {
	vNum := uint32(0x01000000)
	timePoint := uint32(1700000000)
	body := []byte{}
	body = fcraw.AppendVarUint(body, 1) // count
	body = fcraw.AppendVarUint(body, 2) // tag 2 = hardfork_version_vote
	body = append(body, byte(vNum), byte(vNum>>8), byte(vNum>>16), byte(vNum>>24))
	body = append(body, byte(timePoint), byte(timePoint>>8), byte(timePoint>>16), byte(timePoint>>24))
	exts, err := ParseHeaderExtensions(body)
	if err != nil {
		t.Fatalf("ParseHeaderExtensions: %v", err)
	}
	if len(exts) != 1 || exts[0].Tag != HeaderExtHardforkVersionVote {
		t.Fatalf("got %+v, want one HardforkVersionVote entry", exts)
	}
	if exts[0].HardforkVersion != vNum || exts[0].HardforkVersionTime != timePoint {
		t.Errorf("got vNum=%#x time=%d, want %#x / %d",
			exts[0].HardforkVersion, exts[0].HardforkVersionTime, vNum, timePoint)
	}
}
