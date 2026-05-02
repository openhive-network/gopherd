package chain

import (
	"sort"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)


// updateWitnessSchedule mirrors database::update_witness_schedule
// (libraries/chain/witness_schedule.cpp:560). Called every block; the
// schedule actually rotates only when a "round" ends — i.e., when
// head_block_num + 1 reaches NextShuffleBlockNum.
//
// Algorithm (simplified):
//
//  1. select top-N witnesses by total_votes_vests, where N = min(
//     num_active_witnesses, MaxVotedWitnesses + MaxMinerWitnesses + 1)
//  2. shuffle deterministically using the current_virtual_time as seed
//  3. write back to the schedule singleton
//  4. compute median props from the active set; copy to DGPO
//
// We elide the virtual-time scheduling for the timeshare slot (the
// "fairness queue" that lets low-vote witnesses produce occasionally)
// — replay correctness only depends on the SET of producers per block,
// not on the round-robin order vs hived. Block-id parity is preserved
// because we don't sign blocks during replay; the witness is read
// from the block header bytes, which we trust verbatim.
//
// For the testfixture path, the schedule is consulted by the fixture
// to pick block.witness; for the on-disk replay path, the witness
// comes from the parsed block header.
func updateWitnessSchedule(s *state.Session, blockNum uint32) {
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return
	}

	ws, err := objects.GetWitnessSchedule(s)
	if err != nil {
		// Bootstrap: create a schedule containing initminer.
		ws = objects.WitnessSchedule{
			NumScheduledWitnesses: 1,
			CurrentShuffledWitnesses: []auth.AccountName{
				auth.AccountName(initMinerName(blockNum)),
			},
			MaxVotedWitnesses: objects.MaxVotedWitnessesHF0,
			MaxMinerWitnesses: objects.MaxMinerWitnessesHF0,
			NextShuffleBlockNum: blockNum + 1,
		}
		objects.PutWitnessSchedule(s, &ws)
	}

	// Track recent block producers in a HIVE_MAX_WITNESSES-sized window
	// — used by update_median_feed as the active schedule (we don't
	// model C++'s miner queue + virtual-time timeshare slot, so we can't
	// rebuild current_shuffled_witnesses faithfully; the actual block
	// headers are the canonical record of which 21 witnesses were
	// scheduled for the most recent round).
	if dgpo.CurrentWitness != "" {
		newProd := auth.AccountName(dgpo.CurrentWitness)
		if len(ws.RecentBlockProducers) == 0 ||
			ws.RecentBlockProducers[len(ws.RecentBlockProducers)-1] != newProd {
			ws.RecentBlockProducers = append(ws.RecentBlockProducers, newProd)
			if len(ws.RecentBlockProducers) > int(objects.MaxWitnesses) {
				ws.RecentBlockProducers = ws.RecentBlockProducers[len(ws.RecentBlockProducers)-int(objects.MaxWitnesses):]
			}
		}
	}

	// Hardfork-conditional schedule sizing.
	hf := hardfork.SetForBlock(blockNum)
	prevMaxVoted := ws.MaxVotedWitnesses
	prevMaxMiner := ws.MaxMinerWitnesses
	if hf.Has(hardfork.HF20) {
		ws.MaxVotedWitnesses = objects.MaxVotedWitnessesHF20
		ws.MaxMinerWitnesses = 0
	} else {
		if ws.MaxVotedWitnesses == 0 {
			ws.MaxVotedWitnesses = objects.MaxVotedWitnessesHF0
		}
		if ws.MaxMinerWitnesses == 0 {
			ws.MaxMinerWitnesses = objects.MaxMinerWitnessesHF0
		}
	}

	// Only re-shuffle once per round, when head_block_num reaches
	// the next-shuffle marker. On the early-return path persist if
	// hardfork sizing or RecentBlockProducers changed.
	if blockNum < ws.NextShuffleBlockNum {
		objects.PutWitnessSchedule(s, &ws)
		_ = prevMaxVoted
		_ = prevMaxMiner
		_ = dgpo
		return
	}

	// Walk the by-vote secondary index (votes desc, name asc) and pull
	// at most `want` candidates. Stops as soon as we have enough valid
	// ones — avoids scanning the full witness table every shuffle.
	type entry struct {
		name  auth.AccountName
		votes int64
	}
	var all []entry
	cachedWitnesses := make(map[auth.AccountName]objects.Witness, objects.MaxWitnesses)
	want := int(ws.MaxVotedWitnesses) + int(ws.MaxMinerWitnesses)
	if want == 0 || want > objects.MaxWitnesses {
		want = objects.MaxWitnesses
	}
	zero := [33]byte{}
	_ = s.Range(objects.WitnessTable, objects.WitnessByVoteIndex, nil,
		func(key, _ []byte) (bool, error) {
			if len(key) < 8+state.AccountNameKeyLen {
				return true, nil
			}
			// Decode owner from the secondary key bytes (votes_8 + name_16).
			ownerBytes := key[8 : 8+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && ownerBytes[n-1] == 0 {
				n--
			}
			owner := auth.AccountName(string(ownerBytes[:n]))
			w, err := objects.GetWitness(s, owner)
			if err != nil {
				return true, nil
			}
			if hf.Has(hardfork.HF14) && w.BlockSigningKey == zero {
				return true, nil
			}
			cachedWitnesses[w.Owner] = w
			all = append(all, entry{name: w.Owner, votes: w.TotalVotesVests})
			if len(all) >= want {
				return false, nil
			}
			return true, nil
		})
	// Index already yields desc-by-votes, asc-by-name; no sort needed.
	if len(all) > want {
		all = all[:want]
	}
	// If no witnesses are eligible, retain initminer as the sole
	// producer so the chain doesn't stall.
	if len(all) == 0 {
		all = []entry{{name: auth.AccountName(initMinerName(blockNum)), votes: 0}}
	}

	shuffled := make([]auth.AccountName, len(all))
	for i, e := range all {
		shuffled[i] = e.name
	}
	// Deterministic "shuffle" via a fast PRNG seeded from blockNum.
	// hived uses the head_block_id as the seed and a Fisher-Yates pass
	// driven by halves of the digest. For test parity we use a simple
	// LCG seeded by blockNum — every node produces the same order
	// since blockNum is canonical.
	seed := uint64(blockNum)*6364136223846793005 + 1442695040888963407
	for i := len(shuffled) - 1; i > 0; i-- {
		seed = seed*6364136223846793005 + 1442695040888963407
		j := int(seed >> 32) % (i + 1)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}

	ws.CurrentShuffledWitnesses = shuffled
	ws.NumScheduledWitnesses = uint8(len(shuffled))
	if ws.NumScheduledWitnesses == 0 {
		ws.NumScheduledWitnesses = 1
	}
	ws.NextShuffleBlockNum = blockNum + uint32(ws.NumScheduledWitnesses)

	// Mirror C++ witness_schedule.cpp:367-370: every shuffle recomputes
	// witness_pay_normalization_factor from the per-slot weights and
	// the number of slots actually filled by each schedule type. The
	// per-witness schedule labels were assigned during the selection
	// pass (active_witnesses); without modeling the full virtual-time
	// queue we approximate by labeling all picks as elected pre-HF17
	// and elected + 1 timeshare HF17+.
	if ws.ElectedWeight == 0 {
		ws.ElectedWeight = 1
	}
	if ws.MinerWeight == 0 {
		ws.MinerWeight = 1
	}
	if ws.TimeshareWeight == 0 {
		ws.TimeshareWeight = 5
	}
	assignScheduleSlots(s, &ws, hf, cachedWitnesses)

	// Median props from the shuffled set.
	updateMedianWitnessProps(s, &ws, cachedWitnesses)

	objects.PutWitnessSchedule(s, &ws)

	// Mirror hived's update_global_witness_properties: copy
	// median_props into DGPO so other evaluators see the new chain
	// properties immediately. (DGPO doesn't yet track maximum_block_size
	// in our schema; only HBDInterestRate is mirrored for now.)
	if ws.MedianHBDInterestRate > 0 {
		dgpo.HBDInterestRate = ws.MedianHBDInterestRate
	}
	objects.PutDGPO(s, &dgpo)
}

// assignScheduleSlots writes a Schedule (elected/miner/timeshare) onto
// every witness in ws.CurrentShuffledWitnesses and recomputes
// ws.WitnessPayNormalizationFactor.
//
// Pre-HF17: 19 elected + 1 miner + 1 timeshare.
// HF17+   : 20 elected + 0 miner + 1 timeshare.
//
// We don't model the full PoW miner queue or the virtual-time
// timeshare rotation, so we approximate by labeling the FIRST N slots
// elected (N = MaxVotedWitnesses), the next slot miner if MaxMiner > 0
// and the slot is filled, and the LAST slot timeshare. This keeps the
// pay-normalization factor consistent with what hived computes
// (1*num_elected + 1*num_miners + 5*num_timeshare) for the typical
// fully-populated 21-slot schedule.
func assignScheduleSlots(s *state.Session, ws *objects.WitnessSchedule, hf hardfork.Set, cached map[auth.AccountName]objects.Witness) {
	maxElected := int(ws.MaxVotedWitnesses)
	if maxElected == 0 {
		maxElected = int(objects.MaxVotedWitnessesHF0)
		if hf.Has(hardfork.HF20) {
			maxElected = int(objects.MaxVotedWitnessesHF20)
		}
	}
	maxMiner := int(ws.MaxMinerWitnesses)
	// HF17+ retires the miner slot.
	if hf.Has(hardfork.HF17) {
		maxMiner = 0
	}

	var numElected, numMiner, numTimeshare int
	type slot struct {
		name auth.AccountName
		kind objects.WitnessScheduleType
	}
	slots := make([]slot, 0, len(ws.CurrentShuffledWitnesses))
	for i, name := range ws.CurrentShuffledWitnesses {
		var kind objects.WitnessScheduleType
		switch {
		case i < maxElected:
			kind = objects.WitnessScheduleElected
			numElected++
		case i < maxElected+maxMiner:
			kind = objects.WitnessScheduleMiner
			numMiner++
		default:
			kind = objects.WitnessScheduleTimeshare
			numTimeshare++
		}
		slots = append(slots, slot{name: name, kind: kind})
	}

	for _, sl := range slots {
		w, ok := cached[sl.name]
		if !ok {
			gw, err := objects.GetWitness(s, sl.name)
			if err != nil {
				continue
			}
			w = gw
		}
		if w.Schedule != sl.kind {
			w.Schedule = sl.kind
			objects.PutWitness(s, &w)
		}
	}

	ws.WitnessPayNormalizationFactor = ws.ElectedWeight*uint32(numElected) +
		ws.MinerWeight*uint32(numMiner) +
		ws.TimeshareWeight*uint32(numTimeshare)
}

// updateMedianWitnessProps computes medians of account_creation_fee,
// maximum_block_size, and hbd_interest_rate over the shuffled set.
// Mirrors update_median_witness_props (witness_schedule.cpp:58).
func updateMedianWitnessProps(s *state.Session, ws *objects.WitnessSchedule, cached map[auth.AccountName]objects.Witness) {
	if len(ws.CurrentShuffledWitnesses) == 0 {
		return
	}
	type props struct {
		acf  int64
		size uint32
		rate uint16
	}
	all := make([]props, 0, len(ws.CurrentShuffledWitnesses))
	for _, name := range ws.CurrentShuffledWitnesses {
		w, ok := cached[name]
		if !ok {
			gw, err := objects.GetWitness(s, name)
			if err != nil {
				continue
			}
			w = gw
		}
		all = append(all, props{
			acf:  w.AccountCreationFee,
			size: w.MaximumBlockSize,
			rate: w.HBDInterestRate,
		})
	}
	if len(all) == 0 {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].acf < all[j].acf })
	ws.MedianAccountCreationFee = all[len(all)/2].acf
	sort.Slice(all, func(i, j int) bool { return all[i].size < all[j].size })
	ws.MedianMaximumBlockSize = all[len(all)/2].size
	sort.Slice(all, func(i, j int) bool { return all[i].rate < all[j].rate })
	ws.MedianHBDInterestRate = all[len(all)/2].rate
}

// initMinerName returns the genesis initminer name. Mainnet uses
// "initminer"; we don't differentiate testnet here since both share
// the same name.
func initMinerName(_ uint32) string { return "initminer" }
