package chain

import (
	"sort"

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
func updateLastIrreversibleBlock(s *state.Session) {
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

	confirmed := make([]uint32, 0, ws.NumScheduledWitnesses)
	for i := uint8(0); i < ws.NumScheduledWitnesses; i++ {
		if int(i) >= len(ws.CurrentShuffledWitnesses) {
			break
		}
		w, err := objects.GetWitness(s, ws.CurrentShuffledWitnesses[i])
		if err != nil {
			continue
		}
		confirmed = append(confirmed, w.LastConfirmedBlockNum)
	}
	if len(confirmed) == 0 {
		return
	}

	// offset = ((100% - 75%) * N) / 100% — that's the index whose value
	// is the smallest such that >=75% of witnesses have confirmed at or
	// above it (after sorting ascending).
	offset := int((hundredPercentBP - irreversibleThresholdBP) * uint32(len(confirmed)) / hundredPercentBP)
	sort.Slice(confirmed, func(i, j int) bool { return confirmed[i] < confirmed[j] })
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
func updateProducerLastConfirmed(s *state.Session, witness string, blockNum uint32) {
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
	w.LastConfirmedBlockNum = blockNum
	objects.PutWitness(s, &w)
}
