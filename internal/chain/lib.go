package chain

import (
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_IRREVERSIBLE_THRESHOLD = 75% (libraries/protocol/include/hive/protocol/
// config.hpp). The fraction of active witnesses that must have confirmed a
// block before it is considered irreversible.
const irreversibleThresholdBP uint32 = 7500
const hundredPercentBP uint32 = 10000

// updateLastIrreversibleBlock ports
// hive::chain::database::update_last_irreversible_block
// (libraries/chain/database.cpp:2868). Pre-fast-confirm (HF26-) it computes
// LIB as the (75%) median of the active witness schedule's
// last_confirmed_block_num values. Post-fast-confirm uses witness
// fast-confirm transactions, not modeled here yet.
//
// LIB is monotonically non-decreasing — never moves backwards.
func (d *Database) updateLastIrreversibleBlock(s *state.Session) {
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return
	}
	ws, err := objects.GetWitnessSchedule(s)
	if err != nil {
		return
	}
	if ws.NumScheduledWitnesses == 0 {
		return
	}

	d.libConfirmedBuf = d.libConfirmedBuf[:0]
	for i := uint8(0); i < ws.NumScheduledWitnesses; i++ {
		if int(i) >= len(ws.CurrentShuffledWitnesses) {
			break
		}
		name := ws.CurrentShuffledWitnesses[i]
		// Cache hit avoids a btree Get + full Witness deserialize.
		// updateProducerLastConfirmed is the only writer of LCB, so the
		// cache stays consistent without invalidation hooks elsewhere.
		if lcb, ok := d.lcbCache[string(name)]; ok {
			d.libConfirmedBuf = append(d.libConfirmedBuf, lcb)
			continue
		}
		w, err := objects.GetWitness(s, name)
		if err != nil {
			continue
		}
		d.lcbCache[string(name)] = w.LastConfirmedBlockNum
		d.libConfirmedBuf = append(d.libConfirmedBuf, w.LastConfirmedBlockNum)
	}
	confirmed := d.libConfirmedBuf
	if len(confirmed) == 0 {
		return
	}

	// offset = ((100% - 75%) * N) / 100% — that's the index whose value
	// is the smallest such that >=75% of witnesses have confirmed at or
	// above it (after sorting ascending). N≤21, so insertion sort is
	// strictly faster than sort.Slice (which heap-escapes the closure
	// + reflectlite.Swapper allocates 159 MB cum across the run).
	offset := int((hundredPercentBP - irreversibleThresholdBP) * uint32(len(confirmed)) / hundredPercentBP)
	for i := 1; i < len(confirmed); i++ {
		x := confirmed[i]
		j := i
		for ; j > 0 && confirmed[j-1] > x; j-- {
			confirmed[j] = confirmed[j-1]
		}
		confirmed[j] = x
	}
	if offset >= len(confirmed) {
		offset = len(confirmed) - 1
	}
	newLIB := confirmed[offset]
	if newLIB > dgpo.LastIrreversibleBlockNum {
		dgpo.LastIrreversibleBlockNum = newLIB
		objects.PutDGPO(s, &dgpo)
	}
}

// updateProducerLastConfirmed bumps the producing witness's
// last_confirmed_block_num to the just-applied block. Mirrors C++
// update_signing_witness which sets last_confirmed_block_num to the new
// head block number for the producer.
func (d *Database) updateProducerLastConfirmed(s *state.Session, witness string, blockNum uint32) {
	if witness == "" {
		return
	}
	w, err := objects.GetWitness(s, witness)
	if err != nil {
		return
	}
	if w.LastConfirmedBlockNum >= blockNum {
		return
	}
	// Per-block hot path. Snapshot pre-modify index fields so PutWitnessIndexed
	// can skip the read-prev step PutWitness would otherwise do — saves a
	// full Get + ReadWitness round-trip every single block.
	oldVotes := w.TotalVotesVests
	oldVST := w.VirtualScheduledTime
	oldPow := w.PowWorker
	w.LastConfirmedBlockNum = blockNum
	objects.PutWitnessIndexed(s, &w, oldVotes, oldVST, oldPow)
	d.lcbCache[witness] = blockNum
}
