// Package state implements the chainbase-equivalent K/V state layer used
// by the chain (database) and evaluators. It is a Pebble-backed store with
// hived-style table semantics: each "logical collection" of state objects
// (account, witness, comment, …) gets a 16-bit TableID, and each index on
// that collection (primary or secondary) gets an 8-bit IndexID. All
// physical keys are concatenated as
//
//	[TableID big-endian uint16][IndexID uint8][index_specific_key_bytes]
//
// so iteration over a prefix yields one collection's entries in the chosen
// index's sort order. Big-endian table IDs keep the per-table groupings
// contiguous on disk; LSM compaction therefore localizes writes for one
// table to a small number of SSTables.
//
// Sessions buffer writes in memory; reads consult the overlay first and
// fall through to Pebble for everything not yet touched. Commit flushes
// atomically; Undo discards the buffer.
package state

import (
	"encoding/binary"
)

// TableID identifies a logical collection of state objects. The chain plan
// reserves 195 of these (matching the 195 boost::multi_index containers
// hived currently uses); the type is uint16 to leave headroom.
type TableID uint16

// IndexID identifies an index within a table. Convention: 0 is the primary
// index (key → value), and 1..N are secondary indexes (composite_key → empty).
type IndexID uint8

// PrimaryIndex is the conventional IndexID for the primary key.
const PrimaryIndex IndexID = 0

// keyPrefixLen is the size of the [tableID:2][indexID:1] header that
// precedes every physical key.
const keyPrefixLen = 3

// EncodeKey returns the physical key for (table, index, body).
//
// Allocation policy: this function always allocates a fresh slice. Hot-path
// callers should reuse a per-goroutine scratch buffer via EncodeKeyInto.
func EncodeKey(table TableID, index IndexID, body []byte) []byte {
	out := make([]byte, keyPrefixLen+len(body))
	binary.BigEndian.PutUint16(out[:2], uint16(table))
	out[2] = byte(index)
	copy(out[keyPrefixLen:], body)
	return out
}

// EncodeKeyInto writes the physical key into dst[:0] (reusing capacity)
// and returns the resulting slice. Useful in hot paths to avoid per-key
// allocations.
func EncodeKeyInto(dst []byte, table TableID, index IndexID, body []byte) []byte {
	dst = dst[:0]
	dst = append(dst, byte(uint16(table)>>8), byte(uint16(table)))
	dst = append(dst, byte(index))
	dst = append(dst, body...)
	return dst
}

// EncodePrefix returns the [tableID][indexID] header alone — the seek
// prefix for iterating one (table, index) pair.
func EncodePrefix(table TableID, index IndexID) []byte {
	out := make([]byte, keyPrefixLen)
	binary.BigEndian.PutUint16(out[:2], uint16(table))
	out[2] = byte(index)
	return out
}

// DecodeKey splits a physical key into its components.
func DecodeKey(physical []byte) (table TableID, index IndexID, body []byte, ok bool) {
	if len(physical) < keyPrefixLen {
		return 0, 0, nil, false
	}
	table = TableID(binary.BigEndian.Uint16(physical[:2]))
	index = IndexID(physical[2])
	body = physical[keyPrefixLen:]
	return table, index, body, true
}
