package chain

import (
	"encoding/binary"
	"errors"
	"fmt"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_MAX_TIME_UNTIL_EXPIRATION = 1 hour. Mirrors libraries/protocol/
// include/hive/protocol/config.hpp. A tx whose expiration is more than
// 1 hour past head_block_time is rejected (replay-attack prophylaxis).
const maxTimeUntilExpirationSec uint32 = 60 * 60

// validateTxPreFlight runs the structural checks hived's
// _push_transaction performs before evaluator dispatch:
//
//  1. Expiration must not be in the past (head_block_time > expiration).
//  2. Expiration must not be more than HIVE_MAX_TIME_UNTIL_EXPIRATION
//     in the future of head_block_time.
//  3. trx_id must not already be in the dedup window.
//  4. ref_block_num + ref_block_prefix must match the corresponding
//     block_summary_object entry — i.e. the tx references a real block
//     in the recent ring (TaPOS).
//
// On any failure the caller rolls back the per-tx snapshot.
//
// `strictTapos` toggles full prefix-equality enforcement: replay-mode
// callers (canonical block_log already validated by the producing
// network) leave it false; live sync sets it true.
//
// Mirrors database.cpp::_push_transaction at lines 1190-1230 plus
// transaction.cpp::validate.
func validateTxPreFlight(s *state.Session, signed *tx.SignedTransaction, blockNum, headBlockTime uint32, txID [20]byte, strictTapos bool) error {
	// 0) Structural: transaction.validate() asserts operations.size() > 0
	// (transaction.cpp:42). An empty-ops tx can't carry any state change
	// but would still consume a tx-id slot in the dedup set.
	if len(signed.Operations) == 0 {
		return errors.New("transaction has no operations")
	}
	// 1) Expiration in the future of head_block_time AND ≤ +1h.
	// Mirror C++ database.cpp:2535-2538 — HF9 tightened the boundary
	// from `now <= expiration` (pre-HF9) to `now < expiration` (post-HF9).
	// Gopherd's prior `expiration < headBlockTime` accepted equality at
	// ALL times, matching the pre-HF9 behavior even post-HF9.
	hfTx := hardfork.SetForBlock(blockNum)
	if hfTx.Has(hardfork.HF9) {
		if signed.Expiration <= headBlockTime {
			return fmt.Errorf("transaction expired (HF9+): expiration=%d head=%d", signed.Expiration, headBlockTime)
		}
	} else if signed.Expiration < headBlockTime {
		return fmt.Errorf("transaction expired: expiration=%d head=%d", signed.Expiration, headBlockTime)
	}
	if signed.Expiration > headBlockTime+maxTimeUntilExpirationSec {
		return fmt.Errorf("transaction expiration too far in future: expiration=%d head=%d max_delta=%d",
			signed.Expiration, headBlockTime, maxTimeUntilExpirationSec)
	}

	// 2) Dedup window: reject duplicate trx_id.
	if has, err := objects.HasTransaction(s, txID); err != nil {
		return fmt.Errorf("dedup check: %w", err)
	} else if has {
		return errors.New("duplicate transaction (already in dedup window)")
	}

	// 3) TaPOS: ref_block_num resolves to ring slot (block_num & 0xffff)
	//    of the referenced block. Hived's check is
	//      block_summary_object[trx.ref_block_num].block_id._hash[1]
	//      == trx.ref_block_prefix
	//    The block_id has the form [block_num_BE_4 | sha224_bytes_4_to_19],
	//    and `_hash[1]` is bytes[4..7] interpreted as little-endian uint32
	//    (matches the FC ripemd160 layout on x86 / standard hived deploys).
	// Mirror C++ database.cpp:2545-2549 — TaPoS check fires for ALL
	// ref_block_num values including 0 (which maps to ring slot 0 =
	// block_summary[0] = block 65536's ID). The prior `!= 0` exception
	// allowed a tx with ref_block_num==0 to bypass TaPoS regardless of
	// ref_block_prefix.
	if blockNum > 1 {
		bs, err := getBlockSummaryAtRingSlot(s, signed.RefBlockNum)
		if err != nil || bs.Empty() {
			if strictTapos {
				return fmt.Errorf("tapos: ref_block_num %d not present in block_summary ring", signed.RefBlockNum)
			}
		} else if strictTapos {
			prefix := binary.LittleEndian.Uint32(bs.BlockID[4:8])
			if prefix != signed.RefBlockPrefix {
				return fmt.Errorf("tapos: ref_block_prefix mismatch: tx=%08x summary=%08x ref_block_num=%d",
					signed.RefBlockPrefix, prefix, signed.RefBlockNum)
			}
		}
	}

	return nil
}

// getBlockSummaryAtRingSlot reads the ring entry at the slot index
// `(refBlockNum & 0xffff)`. ringSlot is the low-16-bits of any block
// number that maps to this slot.
func getBlockSummaryAtRingSlot(s *state.Session, refBlockNum uint16) (blockSummaryView, error) {
	bs, err := objects.GetBlockSummary(s, uint32(refBlockNum))
	if err != nil {
		return blockSummaryView{}, err
	}
	return blockSummaryView{BlockID: bs.BlockID}, nil
}

type blockSummaryView struct {
	BlockID [20]byte
}

func (v blockSummaryView) Empty() bool {
	for _, b := range v.BlockID {
		if b != 0 {
			return false
		}
	}
	return true
}
