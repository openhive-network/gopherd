package block

import (
	"crypto/sha256"
	"fmt"
	"sort"

	"gopherd/internal/fcraw"
)

// BlockSigningDigest is the message hash a witness signs to produce
// signed_block_header.witness_signature: SHA-256 of the serialized
// (unsigned) block header bytes. Mirrors hived's
// `digest_type::hash(uncompressed_block.raw_bytes, block_header_size)`
// in libraries/chain/full_block.cpp:144.
//
// Important: blocks do NOT mix in chain_id — that's a transaction-only
// thing. The witness_object's signing_key is a single key (no
// network-binding), so cross-chain replay protection here relies on
// previous-block-id chain continuity, not on the digest's preimage.
func BlockSigningDigest(headerBytes []byte) [32]byte {
	return sha256.Sum256(headerBytes)
}

// block_header_extensions is a
// flat_set<static_variant<void_t, version, hardfork_version_vote, state_stamp>>.
// Wire size of each variant body, indexed by tag:
//
//	tag 0 = void_t                  → 0 bytes
//	tag 1 = version                 → 4 bytes (uint32 v_num LE)
//	tag 2 = hardfork_version_vote   → 8 bytes (uint32 v_num LE + uint32 time_point_sec LE)
//	tag 3 = state_stamp             → 20 bytes (checksum_type merkle_root, ripemd160)
//
// state_stamp was added by hived 18045a0ee (HF29 state stamps). Before it a
// tag-3 entry was unparseable to hived as well; whether one is ALLOWED in a
// block is a consensus rule (chain.processHeaderExtensions), not a wire one.
var blockHeaderExtensionBodySize = [...]int{0, 4, 8, 20}

// HeaderExtensionTag enumerates the static_variant tags inside
// block_header_extensions.
type HeaderExtensionTag uint8

const (
	HeaderExtVoid                HeaderExtensionTag = 0
	HeaderExtVersion             HeaderExtensionTag = 1
	HeaderExtHardforkVersionVote HeaderExtensionTag = 2
	HeaderExtStateStamp          HeaderExtensionTag = 3
)

// HeaderExtension is a typed view of one entry in block_header_extensions.
// Only the variant indicated by Tag carries a meaningful payload.
type HeaderExtension struct {
	Tag                 HeaderExtensionTag
	Version             uint32 // packed v_num for HeaderExtVersion
	HardforkVersion     uint32 // packed v_num for HeaderExtHardforkVersionVote
	HardforkVersionTime uint32 // time_point_sec for HeaderExtHardforkVersionVote
	// StateMerkleRoot is state_stamp.merkle_root for HeaderExtStateStamp: the
	// merkle root of the state stamps published since the previous
	// stamp-carrying block.
	StateMerkleRoot [20]byte
}

// ParseHeaderExtensions decodes the opaque Extensions byte range into
// typed entries. Returns nil for the empty case (the common 0x00 marker).
// Mirrors hive::protocol::block_header_extensions static_variant decode.
//
// The result is what hived holds in memory after unpacking, not the wire
// list: fc unpacks a flat_set by insert()ing each element, and
// static_variant's operator< and operator== compare the tag ONLY
// (fc/static_variant.hpp). So entries come back sorted by tag and a repeated
// tag is dropped, the FIRST occurrence winning. That is why hived's
// "Duplicate state stamp block extension" assertion can never fire (its own
// comment in database_witness.cpp says so): a block whose wire list carries
// two state stamps is processed as if it carried only the first.
func ParseHeaderExtensions(raw []byte) ([]HeaderExtension, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	r := fcraw.NewReader(raw)
	count, err := r.ReadVarUint()
	if err != nil {
		return nil, fmt.Errorf("ParseHeaderExtensions count: %w", err)
	}
	if count == 0 {
		return nil, nil
	}
	if err := r.CheckCount(count, 1); err != nil {
		return nil, fmt.Errorf("ParseHeaderExtensions: %w", err)
	}
	out := make([]HeaderExtension, 0, count)
	for i := uint32(0); i < count; i++ {
		tag, err := r.ReadVarUint()
		if err != nil {
			return nil, fmt.Errorf("ParseHeaderExtensions extension %d tag: %w", i, err)
		}
		if int(tag) >= len(blockHeaderExtensionBodySize) {
			return nil, fmt.Errorf("ParseHeaderExtensions extension %d unknown tag %d", i, tag)
		}
		var ext HeaderExtension
		ext.Tag = HeaderExtensionTag(tag)
		switch ext.Tag {
		case HeaderExtVoid:
			// no body
		case HeaderExtVersion:
			v, err := r.ReadUint32()
			if err != nil {
				return nil, fmt.Errorf("ParseHeaderExtensions version body: %w", err)
			}
			ext.Version = v
		case HeaderExtHardforkVersionVote:
			v, err := r.ReadUint32()
			if err != nil {
				return nil, fmt.Errorf("ParseHeaderExtensions hardfork v_num: %w", err)
			}
			t, err := r.ReadUint32()
			if err != nil {
				return nil, fmt.Errorf("ParseHeaderExtensions hardfork time: %w", err)
			}
			ext.HardforkVersion = v
			ext.HardforkVersionTime = t
		case HeaderExtStateStamp:
			if err := r.ReadInto(ext.StateMerkleRoot[:]); err != nil {
				return nil, fmt.Errorf("ParseHeaderExtensions state_stamp merkle_root: %w", err)
			}
		}
		out = insertHeaderExt(out, ext)
	}
	return out, nil
}

// insertHeaderExt is flat_set::insert under static_variant's tag-only
// ordering: keep set sorted by tag, and do nothing if the tag is present.
func insertHeaderExt(set []HeaderExtension, e HeaderExtension) []HeaderExtension {
	i := sort.Search(len(set), func(i int) bool { return set[i].Tag >= e.Tag })
	if i < len(set) && set[i].Tag == e.Tag {
		return set
	}
	set = append(set, HeaderExtension{})
	copy(set[i+1:], set[i:])
	set[i] = e
	return set
}

// AppendHeaderExtensions encodes exts as a block_header_extensions field:
// a varint count followed by one varint tag + fixed-size body per entry.
//
// hived's extensions member is a flat_set< block_header_extensions >, i.e.
// SORTED and DEDUPED, and static_variant compares by tag alone (see
// ParseHeaderExtensions), so a second entry with a tag already present is
// dropped exactly as flat_set::insert would drop it. Callers build the set in
// whatever order the rules read best, so sorting and deduping happens here —
// emitting them out of order would produce a header that round-trips through
// our own parser but differs byte-for-byte from the one hived would have
// signed for the same content.
//
// An empty set encodes as the single 0x00 byte every extension-less block on
// the chain carries.
func AppendHeaderExtensions(dst []byte, exts []HeaderExtension) []byte {
	if len(exts) == 0 {
		return append(dst, 0)
	}
	out := make([]HeaderExtension, 0, len(exts))
	for _, e := range exts {
		out = insertHeaderExt(out, e)
	}

	dst = fcraw.AppendVarUint(dst, uint32(len(out)))
	for _, e := range out {
		dst = fcraw.AppendVarUint(dst, uint32(e.Tag))
		switch e.Tag {
		case HeaderExtVoid:
			// no body
		case HeaderExtVersion:
			dst = appendU32LE(dst, e.Version)
		case HeaderExtHardforkVersionVote:
			dst = appendU32LE(dst, e.HardforkVersion)
			dst = appendU32LE(dst, e.HardforkVersionTime)
		case HeaderExtStateStamp:
			dst = append(dst, e.StateMerkleRoot[:]...)
		}
	}
	return dst
}

func appendU32LE(dst []byte, v uint32) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// SignatureSize is the on-wire size of a witness signature
// (fc::ecc::compact_signature = fc::array<unsigned char, 65>).
const SignatureSize = 65

// Header mirrors hive::protocol::block_header. Field order and types match
// the FC_REFLECT macro in libraries/protocol/include/hive/protocol/block_header.hpp.
type Header struct {
	// Previous is the ID of the prior block. For block 1, this is the
	// all-zero ID.
	Previous ID
	// Timestamp is seconds since unix epoch (matches fc::time_point_sec).
	Timestamp uint32
	// Witness is the witness account name. Block headers store this as a
	// length-prefixed string, NOT a fixed_string<16>; this matches the C++
	// FC_REFLECT( ..., (witness), ... ) which uses `string`.
	Witness string
	// TransactionMerkleRoot is the merkle root of the block's transactions
	// (a SHA-256 hash truncated to 20 bytes via the checksum_type=ripemd160 alias).
	TransactionMerkleRoot [IDSize]byte
	// Extensions is the block_header_extensions flat_set. Empty in the vast
	// majority of mainnet blocks; we model it as raw bytes so we can pass
	// through whatever the wire format produced without fully parsing each
	// extension variant. For round-trip parity this is encoded back verbatim.
	Extensions []byte
}

// SignedHeader is hive::protocol::signed_block_header — Header plus a 65-byte
// witness signature.
type SignedHeader struct {
	Header
	WitnessSignature [SignatureSize]byte
}

// MarshalHeader serializes h to the FC binary format and appends to dst.
// Encoding mirrors FC_REFLECT(block_header, (previous)(timestamp)(witness)(transaction_merkle_root)(extensions)).
func MarshalHeader(dst []byte, h *Header) []byte {
	dst = append(dst, h.Previous[:]...)
	dst = appendUint32LE(dst, h.Timestamp)
	dst = fcraw.AppendVarUint(dst, uint32(len(h.Witness)))
	dst = append(dst, h.Witness...)
	dst = append(dst, h.TransactionMerkleRoot[:]...)
	dst = append(dst, h.Extensions...)
	return dst
}

// UnmarshalSignedHeader parses a signed_block_header from src starting at
// offset 0. Returns the parsed header and the number of bytes consumed (the
// signed-header byte length, which is what gets SHA-224-hashed for the block ID).
//
// Extensions are captured as opaque bytes (the trailing tail of the header
// before the witness signature). For mainnet replay this is overwhelmingly
// the single byte 0x00 = empty flat_set, but we tolerate larger encodings
// without fully parsing each variant.
func UnmarshalSignedHeader(src []byte) (SignedHeader, int, error) {
	r := fcraw.NewReader(src)
	var sh SignedHeader

	// previous: 20 raw bytes
	if err := r.ReadInto(sh.Previous[:]); err != nil {
		return sh, 0, fmt.Errorf("block.UnmarshalSignedHeader previous: %w", err)
	}
	ts, err := r.ReadUint32()
	if err != nil {
		return sh, 0, fmt.Errorf("block.UnmarshalSignedHeader timestamp: %w", err)
	}
	sh.Timestamp = ts
	witness, err := r.ReadString()
	if err != nil {
		return sh, 0, fmt.Errorf("block.UnmarshalSignedHeader witness: %w", err)
	}
	sh.Witness = witness
	if err := r.ReadInto(sh.TransactionMerkleRoot[:]); err != nil {
		return sh, 0, fmt.Errorf("block.UnmarshalSignedHeader merkle_root: %w", err)
	}

	// Extensions: a flat_set<static_variant<void_t, version, hardfork_version_vote, state_stamp>>.
	// Wire format: varint(count) || (varint(tag) || body) repeated.
	// We capture the raw byte range so re-marshalling is bit-identical;
	// for ID computation we don't need the typed values.
	extStart := r.Pos()
	count, err := r.ReadVarUint()
	if err != nil {
		return sh, 0, fmt.Errorf("block.UnmarshalSignedHeader extensions count: %w", err)
	}
	for i := uint32(0); i < count; i++ {
		tag, err := r.ReadVarUint()
		if err != nil {
			return sh, 0, fmt.Errorf("block.UnmarshalSignedHeader extension %d tag: %w", i, err)
		}
		if int(tag) >= len(blockHeaderExtensionBodySize) {
			return sh, 0, fmt.Errorf("block.UnmarshalSignedHeader extension %d unknown tag %d", i, tag)
		}
		bodyLen := blockHeaderExtensionBodySize[tag]
		if r.Remaining() < bodyLen {
			return sh, 0, fmt.Errorf("block.UnmarshalSignedHeader extension %d truncated body (need %d, have %d)", i, bodyLen, r.Remaining())
		}
		if _, err := r.ReadRawAlias(bodyLen); err != nil {
			return sh, 0, fmt.Errorf("block.UnmarshalSignedHeader extension %d body: %w", i, err)
		}
	}
	extEnd := r.Pos()
	sh.Extensions = append([]byte(nil), src[extStart:extEnd]...)

	if err := r.ReadInto(sh.WitnessSignature[:]); err != nil {
		return sh, 0, fmt.Errorf("block.UnmarshalSignedHeader signature: %w", err)
	}

	return sh, r.Pos(), nil
}

// MarshalSignedHeader serializes a signed_block_header to FC binary form.
func MarshalSignedHeader(dst []byte, sh *SignedHeader) []byte {
	dst = MarshalHeader(dst, &sh.Header)
	dst = append(dst, sh.WitnessSignature[:]...)
	return dst
}

// appendUint32LE — kept local until we promote fcraw.Writer's append-style
// API to a free-function form. Performance-equivalent to
// binary.LittleEndian.AppendUint32 but avoids the import.
func appendUint32LE(dst []byte, v uint32) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}
