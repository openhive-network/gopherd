package sync

import (
	"hived-go/internal/p2p/node"
	"hived-go/internal/p2p/proto"
	"hived-go/internal/protocol/block"
)

// handleCompressedBlock parses a compressed_block_message body, decompresses
// it via the shared blocklog/zstd Decompressor, derives the canonical
// block_id, and feeds the uncompressed bytes into the same applyJob
// pipeline as a regular BlockMessage. Mirror of hived's
// message::as_compressed_block_message + create_from_compressed_block_data
// path (libraries/net/core_messages.cpp:133, libraries/chain/full_block.cpp).
//
// The two-byte wire prefix is the same one block_log uses for its on-disk
// trailer attribute encoding:
//
//	byte 0  bit 7 → zstd flag (0x80)
//	        bit 6 → "dictionary number follows" flag (0x40)
//	byte 1  uint8 dictionary number when bit 6 set, else don't care
//	bytes 2.. raw magicless zstd payload
//
// We DO NOT mirror hived's "uncompressed compressed_block_message" path
// (zstd flag clear): hived itself never sends those — see
// peer_connection.cpp::send_block which only chooses compressed when the
// peer advertised a sufficient last_available_zstd_compression_dictionary_number.
func (d *Delegate) handleCompressedBlock(p *node.Peer, body []byte) {
	d.compressedReceived.Add(1)
	var msg proto.CompressedBlockMessage
	if err := msg.Unmarshal(body); err != nil {
		d.compressedDecodeErrors.Add(1)
		d.logf("compressed_block_message decode: %v", err)
		return
	}
	if !msg.Zstd {
		// Hived's own compressor only ever clears the flag for an
		// "uncompressed compressed-block" — which is just the raw
		// signed_block bytes. Forward as if it were a BlockMessage body
		// (sans trailing block_id, which we'll synthesize).
		d.dispatchUncompressedFromCompressed(p, msg.CompressedBytes)
		return
	}
	raw, err := d.decompressCompressedBlock(msg)
	if err != nil {
		d.compressedDecodeErrors.Add(1)
		d.logf("compressed_block_message decompress: %v", err)
		return
	}
	d.dispatchUncompressedFromCompressed(p, raw)
}

// decompressCompressedBlock dispatches by dictionary flag. When no
// dictionary is signalled (bit 6 clear) we fall back to the dictionary-less
// decoder; this matches hived's full_block::decompress_block, which honours
// the per-block attributes regardless of source.
func (d *Delegate) decompressCompressedBlock(msg proto.CompressedBlockMessage) ([]byte, error) {
	d.compressedDecompressed.Add(1)
	if !msg.HasDictionary {
		return d.decompressor.DecompressNoDict(msg.CompressedBytes, nil)
	}
	return d.decompressor.Decompress(msg.CompressedBytes, msg.DictionaryNumber, nil)
}

// dispatchUncompressedFromCompressed feeds the decompressed signed_block
// bytes into the same pipeline handleBlock uses. Differences vs handleBlock:
//
//  1. The wire body for a compressed_block_message has NO trailing 20-byte
//     block_id; we derive it from the signed-block-header bytes.
//  2. We dedup against d.recentCompSeen so a duplicate compressed delivery
//     (peer A and peer B both pushing the same block) doesn't decompress
//     twice. The dedup is best-effort: the secondary check against
//     headBlockHint + recent-block cache in handleBlockBytes still catches
//     the case where applyLoop already finished and the entry slid out of
//     the small ring.
func (d *Delegate) dispatchUncompressedFromCompressed(p *node.Peer, raw []byte) {
	if len(raw) == 0 {
		d.compressedDecodeErrors.Add(1)
		return
	}
	id, ok := blockIDFromRaw(raw)
	if !ok {
		d.compressedDecodeErrors.Add(1)
		d.logf("compressed_block_message: header parse failed")
		return
	}
	// Dedup ring — short-circuit duplicate compressed deliveries before
	// we touch the apply queue. Note: we already paid for one decompress
	// per ID in decompressCompressedBlock; the cache here only protects
	// against fan-out where the same ID arrives a SECOND time before
	// applyLoop runs. After apply, headBlockHint covers further dups for
	// free in handleBlockBytes.
	if d.markCompressedSeen(id) {
		d.compressedCacheHits.Add(1)
		// Duplicate compressed delivery (the walk-forward fanned the
		// fetch out to every peer, so the same block arrives from several
		// of them). We short-circuit before handleBlockBytes here, so we
		// must release this peer's outstanding-fetch slot ourselves —
		// otherwise the in-flight budget leaks one slot per duplicate and
		// catch-up stalls once MaxOutstandingTotal is reached.
		d.releaseOutstanding(p, id)
		return
	}
	d.handleBlockBytes(p, raw, id)
}

// markCompressedSeen returns true if id was already in the ring (caller
// should treat the inbound copy as a duplicate). Otherwise records id and
// returns false. Maintains a FIFO of recentCompressedCap entries.
func (d *Delegate) markCompressedSeen(id block.ID) bool {
	d.recentCompMu.Lock()
	defer d.recentCompMu.Unlock()
	if _, ok := d.recentCompSeen[id]; ok {
		return true
	}
	if len(d.recentCompFIFO) >= recentCompressedCap {
		evict := d.recentCompFIFO[0]
		d.recentCompFIFO = d.recentCompFIFO[1:]
		delete(d.recentCompSeen, evict)
	}
	d.recentCompFIFO = append(d.recentCompFIFO, id)
	d.recentCompSeen[id] = struct{}{}
	return false
}

// blockIDFromRaw derives the canonical 20-byte block_id from a freshly
// decompressed signed_block byte sequence. Mirrors gossip's helper but
// is duplicated here so the sync package doesn't depend on gossip.
func blockIDFromRaw(rawBlock []byte) (block.ID, bool) {
	sh, hdrLen, err := block.UnmarshalSignedHeader(rawBlock)
	if err != nil {
		return block.ID{}, false
	}
	blockNum := sh.Previous.Num() + 1
	return block.ConstructID(rawBlock[:hdrLen], blockNum), true
}
