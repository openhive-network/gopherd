package chain

import (
	"fmt"
	"os"

	"gopherd/internal/protocol/hardfork"
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
