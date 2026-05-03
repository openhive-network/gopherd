package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/state"
)

// BlockSummaryTable is the ring buffer of recent block IDs keyed by
// (block_num & 0xffff). Mirrors hive::chain::block_summary_object
// (libraries/chain/include/hive/chain/detail/state/block_summary_object.hpp).
//
// Rationale: TaPOS (transaction-as-proof-of-stake) embeds in each
// transaction a `(ref_block_num, ref_block_prefix)` pair pointing at a
// recent block. Validation fetches `block_summary[ref_block_num & 0xffff]`
// and checks that ref_block_prefix matches bytes 4-7 of the stored
// block_id. The ring is 0x10000 = 65536 entries deep, sized so that
// the max-tx-expiration window (~1h) cannot wrap.
const BlockSummaryTable state.TableID = 25

// blockSummaryKey encodes ring index `(block_num & 0xffff)` as 2 bytes BE
// so byte-lex order matches numeric.
func blockSummaryKey(blockNum uint32) []byte {
	idx := uint16(blockNum & 0xffff)
	return []byte{byte(idx >> 8), byte(idx)}
}

// BlockSummary is one ring entry: the 20-byte block_id of the block
// whose number, modulo 0x10000, equals the row key.
type BlockSummary struct {
	BlockID [20]byte
}

func AppendBlockSummary(dst []byte, b *BlockSummary) []byte {
	return append(dst, b.BlockID[:]...)
}

func ReadBlockSummary(src []byte) (BlockSummary, error) {
	var b BlockSummary
	if len(src) < 20 {
		return b, fmt.Errorf("block_summary: short read (%d bytes)", len(src))
	}
	copy(b.BlockID[:], src[:20])
	if len(src) > 20 {
		return b, errors.New("block_summary: trailing bytes")
	}
	return b, nil
}

// PutBlockSummary stores the block id for ring slot `(block_num & 0xffff)`.
// Per-block apply hook calls this after the block is fully applied so the
// next block's transactions can validate TaPOS against it.
func PutBlockSummary(s *state.Session, blockNum uint32, blockID [20]byte) {
	b := BlockSummary{BlockID: blockID}
	s.Put(BlockSummaryTable, state.PrimaryIndex, blockSummaryKey(blockNum), AppendBlockSummary(nil, &b))
}

// GetBlockSummary fetches the ring entry for `(block_num & 0xffff)`.
// Returns ErrNotFound if the slot has not been populated yet (early
// chain — before block 0x10000 fills the ring).
func GetBlockSummary(s *state.Session, blockNum uint32) (BlockSummary, error) {
	v, err := s.Get(BlockSummaryTable, state.PrimaryIndex, blockSummaryKey(blockNum))
	if err != nil {
		return BlockSummary{}, err
	}
	return ReadBlockSummary(v)
}
