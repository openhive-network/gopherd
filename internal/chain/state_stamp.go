package chain

import (
	"fmt"
	"os"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/tx"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// publishStateStamp is state_stamp_data_object::publish: append one digest
// to the stamps waiting for the next stamp-carrying block.
//
// hived creates the object at genesis (e78bfabd1). gopherd does the same
// (genesis.Apply), but a state written before gopherd kept the list has no
// row. A missing row at the first stamp of the chain's history is harmless —
// nothing was missed — so it is created clean; anywhere later it is created
// Incomplete, which makes the HF29 check refuse the state rather than build
// a root over the wrong list.
func publishStateStamp(s *state.Session, blockNum uint32, digest [32]byte) {
	d, ok, err := objects.GetStateStampData(s)
	if err != nil {
		// A row that cannot be decoded is not a list we can append to.
		d = objects.StateStampData{Incomplete: true}
		fmt.Fprintf(os.Stderr, "chain: WARN state_stamp_data at block %d unreadable (%v); marking it incomplete\n", blockNum, err)
	} else if !ok {
		d.Incomplete = !isFirstRCStatsReport(blockNum)
		if d.Incomplete {
			fmt.Fprintf(os.Stderr, "chain: WARN state was written without the state stamp list; "+
				"it cannot validate the first HF29 block. Replay from before HF20 (block %d) to rebuild it.\n",
				hardfork.MainnetActivationBlock[hardfork.HF20])
		}
	}
	d.Stamps = append(d.Stamps, digest)
	objects.PutStateStampData(s, &d)
}

// isFirstRCStatsReport reports whether the RC stats report at blockNum is the
// first since HF20, i.e. no stamp was published before it. The report runs
// every rcStatsReportFreqBlocks blocks once resource_credits::finalize_block
// runs (HF20 active for the block).
func isFirstRCStatsReport(blockNum uint32) bool {
	if blockNum <= rcStatsReportFreqBlocks {
		return true
	}
	return !hardfork.SetForBlock(blockNum - rcStatsReportFreqBlocks).Has(hardfork.HF20)
}

// StateStampRoot is state_stamp_data_object::finalize: the merkle root of
// the stamps in publication order, by hived's calculate_merkle_root
// (libraries/protocol/merkle.cpp, 1cf2455d8), which is the very routine
// that computes a block's transaction_merkle_root: pairwise SHA-256 of
// 64-byte concatenations, an odd layer's last digest carried up unchanged,
// then RIPEMD-160 of the last digest. The empty list gives the zero
// checksum, but a block never carries the root of an empty list.
func StateStampRoot(stamps [][32]byte) [20]byte {
	digests := make([]tx.Digest, len(stamps))
	for i := range stamps {
		digests[i] = tx.Digest(stamps[i])
	}
	return tx.MerkleRoot(digests)
}

// errIncompleteStateStamps explains a state that cannot take part in HF29
// state stamping: it lacks stamps published before it learned to keep them.
func errIncompleteStateStamps() error {
	return fmt.Errorf("state stamp list is incomplete: this state was written by a gopherd that did not keep "+
		"the stamps published since HF20, so it cannot compute the root HF29 blocks carry; "+
		"replay from before HF20 (block %d) to rebuild it", hardfork.MainnetActivationBlock[hardfork.HF20])
}

// PendingStateStampRoot returns the root the next block has to carry under
// HF29, and whether it has to carry one at all (stamps are pending). This is
// what hived's block_producer::add_state_stamp reads. It fails for a state
// whose list is missing or Incomplete.
func PendingStateStampRoot(s *state.Session) (root [20]byte, pending bool, err error) {
	d, ok, err := objects.GetStateStampData(s)
	if err != nil {
		return root, false, fmt.Errorf("state_stamp_data: %w", err)
	}
	if !ok || d.Incomplete {
		return root, false, errIncompleteStateStamps()
	}
	if len(d.Stamps) == 0 {
		return root, false, nil
	}
	return StateStampRoot(d.Stamps), true, nil
}

// checkStateStampExtension is the HF29 half of hived's
// process_header_extensions (18045a0ee): every node has collected the same
// stamps since the previous stamp-carrying block, so a block must carry
// their root exactly when some are pending, the root must be theirs, and
// once it is accepted the list starts over.
//
// stamp is the block's state_stamp extension, nil when it has none. The
// pre-HF29 refusal of the extension and the (unreachable, see
// block.ParseHeaderExtensions) duplicate check happen in the caller.
func checkStateStampExtension(s *state.Session, stamp *[20]byte) error {
	expected, pending, err := PendingStateStampRoot(s)
	if err != nil {
		return err
	}
	if !pending {
		if stamp != nil {
			return fmt.Errorf("Unexpected state stamp block extension")
		}
		return nil
	}
	if stamp == nil {
		return fmt.Errorf("Block is missing the required state stamp block extension")
	}
	if *stamp != expected {
		return fmt.Errorf("State stamp mismatch - node diverged on stamped consensus state, got %x, expected %x", *stamp, expected)
	}
	objects.PutStateStampData(s, &objects.StateStampData{})
	return nil
}
