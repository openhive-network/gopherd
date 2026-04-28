// Package block defines the on-wire block_header / signed_block_header types
// and the rules for computing block IDs.
//
// block_id construction (libraries/chain/full_block.cpp:construct_block_id):
//
//  1. SHA-224 the entire signed_block_header byte range.
//  2. Overwrite the first 4 bytes of the digest with block_num in big-endian
//     order. The C++ code does this by writing
//     fc::endian_reverse_u32(block_num) into the first uint32 of a
//     little-endian-laid-out hash buffer.
//  3. Truncate to the first 20 bytes.
//
// The resulting 20-byte ID is opaque except that bytes [0:4] always equal
// block_num in big-endian, so num_from_id is just a 4-byte BE read.
//
// The hash here is SHA-224, NOT RIPEMD-160. The C++ type is named
// `fc::ripemd160` solely because both produce 20-byte outputs and the type
// alias is reused as a fixed 20-byte container — the actual hash inside a
// block_id is sha224's first 20 bytes, not ripemd160.
package block

import (
	"encoding/binary"

	"hived-go/internal/crypto"
)

// IDSize is the on-disk / in-memory size of a Hive block ID.
const IDSize = 20

// ID is a 20-byte block identifier. The first 4 bytes are block_num in big-
// endian; the remaining 16 are the next bytes of SHA-224(signed_block_header).
type ID [IDSize]byte

// Num returns the block number embedded in the first 4 bytes of id.
func (id ID) Num() uint32 {
	return binary.BigEndian.Uint32(id[:4])
}

// Equal reports whether id == other.
func (id ID) Equal(other ID) bool { return id == other }

// ConstructID computes the block ID for a signed_block_header given its full
// serialized bytes (the bytes that were SHA-224 hashed, i.e. block_header +
// witness_signature) and the block number.
//
// signedHeader must be exactly the byte range hived treats as the signed
// header — bytes from the start of the block up to and including the 65-byte
// witness_signature. transactions follow but must NOT be included.
func ConstructID(signedHeader []byte, blockNum uint32) ID {
	digest := crypto.SHA224(signedHeader)
	var id ID
	copy(id[:], digest[:IDSize])
	binary.BigEndian.PutUint32(id[:4], blockNum)
	return id
}
