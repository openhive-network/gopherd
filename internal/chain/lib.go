package chain

import (
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
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
	dgpoRef, ok := objects.GetDGPORef(s)
	if !ok {
		return
	}
	wsRef, ok := objects.GetWitnessScheduleRef(s)
	if !ok {
		return
	}
	// database.cpp:2928-2934 get_witness_schedule_object_for_irreversibility:
	// from HF26 (FUTURE_WITNESS_SCHEDULE) the 21 opinions that count are the
	// FUTURE schedule's, not the current one's. Sampling the current set put
	// gopherd's last_irreversible_block_num a few blocks off hived's at round
	// boundaries, which moves fork-db pruning and finality timing.
	if hardfork.SetForBlock(dgpoRef.HeadBlockNumber).Has(hardfork.HF26) {
		if fws, ok := objects.EnsureFutureWitnessScheduleStore(s).GetRef(); ok {
			wsRef = fws
		}
	}
	if wsRef.NumScheduledWitnesses == 0 {
		return
	}

	d.libConfirmedBuf = d.libConfirmedBuf[:0]
	// Refresh the cached *Witness slice if the schedule has rotated.
	// CurrentShuffledWitnesses changes every 21 blocks (once per
	// schedule rebuild); between rebuilds the slice is stable so the
	// cache hits on every per-block LIB update.
	d.refreshSchedWitPtrs(s, wsRef.CurrentShuffledWitnesses)
	for _, w := range d.schedWitPtrs {
		if w == nil {
			continue
		}
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
	if newLIB > dgpoRef.LastIrreversibleBlockNum {
		objects.MutateDGPO(s, func(p *objects.DynamicGlobalProperty) {
			p.LastIrreversibleBlockNum = newLIB
		})
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
	// Per-block hot path. Fires from ApplyBlock AFTER DiscardSavepoint,
	// so UndoEnabled is always false here. GetWitnessRef + in-place
	// pointer mutation: LastConfirmedBlockNum isn't a secondary-index
	// key so no flushSecondaries dirty marking is needed. Skip the
	// PutWitnessIndexed (~700-byte Witness value-copy) entirely.
	wRef, ok := objects.GetWitnessRef(s, auth.AccountName(witness))
	if !ok {
		return
	}
	if wRef.LastConfirmedBlockNum >= blockNum {
		return
	}
	wRef.LastConfirmedBlockNum = blockNum
	d.lcbCache[witness] = blockNum
}

// refreshSchedWitPtrs rebuilds the cached *Witness slice when the
// schedule's CurrentShuffledWitnesses has rotated. Compares the slice
// header against the cached snapshot — when the underlying array is
// the same (no rotation since last call), no work is done.
func (d *Database) refreshSchedWitPtrs(s *state.Session, current []auth.AccountName) {
	if sameNameSlice(d.schedWitPtrsKey, current) {
		return
	}
	if cap(d.schedWitPtrs) < len(current) {
		d.schedWitPtrs = make([]*objects.Witness, len(current))
		d.schedWitPtrsKey = make([]auth.AccountName, len(current))
	} else {
		d.schedWitPtrs = d.schedWitPtrs[:len(current)]
		d.schedWitPtrsKey = d.schedWitPtrsKey[:len(current)]
	}
	for i, name := range current {
		d.schedWitPtrsKey[i] = name
		if w, ok := objects.GetWitnessRef(s, name); ok {
			d.schedWitPtrs[i] = w
		} else {
			d.schedWitPtrs[i] = nil
		}
	}
}

func sameNameSlice(a, b []auth.AccountName) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
