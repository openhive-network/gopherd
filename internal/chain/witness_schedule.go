package chain

import (
	"fmt"
	"math/big"
	"os"
	"sort"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_HARDFORK_REQUIRED_WITNESSES — the number of scheduled witnesses
// that must run a given version (or vote a given hardfork) for it to
// take effect. From libraries/protocol/include/hive/protocol/config.hpp.
const hiveHardforkRequiredWitnesses uint8 = 17

// initMinerName returns the genesis initminer name. Mainnet uses
// "initminer"; we don't differentiate testnet here since both share
// the same name.
func initMinerName(_ uint32) string { return "initminer" }

// updateWitnessSchedule mirrors database::update_witness_schedule
// (libraries/chain/witness_schedule.cpp:403). Called every block; the
// schedule actually rotates only when (head_block_num % HIVE_MAX_WITNESSES)
// == 0 — i.e., once per round (every 21 blocks).
//
// Two scheduling algorithms exist:
//   - Pre-HF4: legacy path that locks out the top max_voted-2 witnesses
//     by setting their VirtualScheduledTime to MAX, then picks ONE
//     timeshare witness from by_schedule_time, then drains the PoW
//     queue into the remaining slots.
//   - Post-HF4: update_witness_schedule_4 — picks max_voted by votes,
//     max_miner from PoW queue, fills remaining slots from
//     by_schedule_time, advances each picked timeshare's VST by
//     LAP_LENGTH2 / (votes+1).
//
// Both algorithms then xorshift-shuffle the active set, recompute
// median witness props, and update next_shuffle_block_num.
func updateWitnessSchedule(s *state.Session, blockNum uint32) {
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return
	}

	ws, err := objects.GetWitnessSchedule(s)
	if err != nil || ws.NumScheduledWitnesses == 0 {
		// Bootstrap: create a schedule containing initminer.
		ws = objects.WitnessSchedule{
			NumScheduledWitnesses: 1,
			CurrentShuffledWitnesses: []auth.AccountName{
				auth.AccountName(initMinerName(blockNum)),
			},
			MaxVotedWitnesses:         objects.MaxVotedWitnessesHF0,
			MaxMinerWitnesses:         objects.MaxMinerWitnessesHF0,
			MaxRunnerWitnesses:        1,
			HardforkRequiredWitnesses: hiveHardforkRequiredWitnesses,
			NextShuffleBlockNum:       blockNum + 1,
			ElectedWeight:             1,
			MinerWeight:               1,
			TimeshareWeight:           5,
		}
		objects.PutWitnessSchedule(s, &ws)
	}

	// Track recent block producers for backward compatibility — the
	// median feed reads CurrentShuffledWitnesses now, but the ring is
	// kept around for any legacy debugging code that reads it.
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

	// Hardfork-conditional schedule sizing (mirrors witness_schedule_object
	// activation logic at hardfork apply sites).
	hf := hardfork.SetForBlock(blockNum)
	prevMaxVoted := ws.MaxVotedWitnesses
	prevMaxMiner := ws.MaxMinerWitnesses
	prevMaxRunner := ws.MaxRunnerWitnesses
	if hf.Has(hardfork.HF20) {
		ws.MaxVotedWitnesses = objects.MaxVotedWitnessesHF20
		ws.MaxMinerWitnesses = 0
		ws.MaxRunnerWitnesses = 1
	} else if hf.Has(hardfork.HF17) {
		// HF17 raises elected from 19 → 20 and drops miners.
		ws.MaxVotedWitnesses = 20
		ws.MaxMinerWitnesses = 0
		ws.MaxRunnerWitnesses = 1
	} else {
		if ws.MaxVotedWitnesses == 0 {
			ws.MaxVotedWitnesses = objects.MaxVotedWitnessesHF0
		}
		if ws.MaxMinerWitnesses == 0 {
			ws.MaxMinerWitnesses = objects.MaxMinerWitnessesHF0
		}
		if ws.MaxRunnerWitnesses == 0 {
			ws.MaxRunnerWitnesses = 1
		}
	}
	if ws.HardforkRequiredWitnesses == 0 {
		ws.HardforkRequiredWitnesses = hiveHardforkRequiredWitnesses
	}
	if ws.ElectedWeight == 0 {
		ws.ElectedWeight = 1
	}
	if ws.MinerWeight == 0 {
		ws.MinerWeight = 1
	}
	if ws.TimeshareWeight == 0 {
		ws.TimeshareWeight = 5
	}

	// Round-boundary check: hived's update_witness_schedule
	// (witness_schedule.cpp:511) gates on `head_block_num %
	// HIVE_MAX_WITNESSES == 0` — a HARDCODED 21, *not* the live
	// num_scheduled_witnesses. The commented-out `wso.next_shuffle_block_num`
	// in that file shows that's the older behavior that was replaced.
	// Using the dynamic value here makes Go rebuild more often when the
	// schedule is below 21 (early chain, etc.), which means Go runs
	// over-cap drains at rebuilds C++ skips, which permanently shrinks
	// Go's by_pow vs C++'s and feeds back into divergent miner picks.
	const rebuildPeriod = uint32(objects.MaxWitnesses) // 21
	if blockNum%rebuildPeriod != 0 {
		if ws.MaxVotedWitnesses != prevMaxVoted ||
			ws.MaxMinerWitnesses != prevMaxMiner ||
			ws.MaxRunnerWitnesses != prevMaxRunner ||
			dgpo.CurrentWitness != "" {
			objects.PutWitnessSchedule(s, &ws)
		}
		_ = dgpo
		return
	}

	if hf.Has(hardfork.HF4) {
		updateWitnessSchedule4(s, &ws, &dgpo, blockNum, hf)
	} else {
		updateWitnessSchedulePreHF4(s, &ws, &dgpo, blockNum, hf)
	}

	// HF20+: refresh the rc_pool_object's per-resource params from the
	// new median chain_properties. Mirrors hived's
	// db.rc().set_pool_params(wso) at update_witness_schedule_4. Pre-HF20
	// the call short-circuits inside.
	rcSetPoolParams(s, blockNum, dgpo.Time)

	objects.PutWitnessSchedule(s, &ws)
	objects.PutDGPO(s, &dgpo)
}

// updateWitnessSchedule4 ports libraries/chain/witness_schedule.cpp:152.
// Picks max_voted_witnesses by votes, max_miner_witnesses from the PoW
// queue, then fills the remaining slots from by_schedule_time
// (smallest VirtualScheduledTime first). Each timeshare-picked witness
// has its VST advanced by LAP_LENGTH2 / (votes+1). On overflow the
// chain-wide current_virtual_time and every witness's VST are reset to
// 0 via reset_virtual_schedule_time.
func updateWitnessSchedule4(s *state.Session, ws *objects.WitnessSchedule, dgpo *objects.DynamicGlobalProperty, blockNum uint32, hf hardfork.Set) {
	zero33 := [33]byte{}

	type picked struct {
		owner auth.AccountName
		kind  objects.WitnessScheduleType
	}
	var active []picked

	// 1) Top max_voted by votes (skipping disabled witnesses post-HF14).
	//    Two-pass: collect candidates from the by-vote index, then mutate
	//    Schedule + persist outside the iteration so we don't mutate the
	//    index we're scanning.
	wstore := objects.EnsureWitnessStore(s)
	selectedVoted := make(map[string]bool)
	type candidate struct {
		owner auth.AccountName
	}
	var voteCands []candidate
	_ = wstore.RangeByVote(func(w *objects.Witness) (bool, error) {
		if len(voteCands) >= int(objects.MaxWitnesses) {
			return false, nil
		}
		voteCands = append(voteCands, candidate{owner: w.Owner})
		return true, nil
	})
	for _, c := range voteCands {
		if uint8(len(selectedVoted)) >= ws.MaxVotedWitnesses {
			break
		}
		w, err := objects.GetWitness(s, c.owner)
		if err != nil {
			continue
		}
		if hf.Has(hardfork.HF14) && w.BlockSigningKey == zero33 {
			continue
		}
		selectedVoted[string(w.Owner)] = true
		active = append(active, picked{owner: w.Owner, kind: objects.WitnessScheduleElected})
		w.Schedule = objects.WitnessScheduleElected
		objects.PutWitness(s, &w)
	}

	// Comprehensive per-rebuild full-state dump matching hived's
	// placement (right after voted iter, before miner + timeshare passes).
	if dumpPath := os.Getenv("GHIVEDIFF_DUMP_FILE"); dumpPath != "" {
		dumpFullWitnessTSV(s, blockNum, ws, dumpPath)
	}
	// Legacy block-list-gated dumps kept for compatibility.
	if envGhivediffVsts != "" {
		if (blockNum >= 1041495 && blockNum <= 1050000) ||
			(blockNum >= 2880000 && blockNum <= 2910000) ||
			(blockNum%21000 == 0 && blockNum >= 1050000 && blockNum <= 2888500) {
			dumpSchedVSTs(s, blockNum)
		}
	}

	// 2) Drain top max_miner_witnesses from by_pow.
	//    Pre-HF13 only — post-HF13 num_pow_witnesses is 0 and the
	//    max_miner_witnesses count is 0 for HF17+ anyway.
	//    Two-pass: collect candidates first, then mutate.
	selectedMiners := make(map[string]bool)
	type powEntry struct {
		owner auth.AccountName
		pow   uint64
	}
	var powCandidates []powEntry
	_ = wstore.RangeByPow(func(w *objects.Witness) (bool, error) {
		// Stop once we've harvested enough candidates to fill all
		// max_miner slots — matches C++'s mitr advance loop, which
		// only drops witnesses as far as max_miner_witnesses.
		if uint8(len(selectedMiners)) >= ws.MaxMinerWitnesses && len(powCandidates) >= int(ws.MaxMinerWitnesses) {
			return false, nil
		}
		if w.PowWorker == 0 {
			return true, nil
		}
		owner := w.Owner
		powCandidates = append(powCandidates, powEntry{owner: owner, pow: w.PowWorker})
		if !selectedVoted[string(owner)] {
			if !(hf.Has(hardfork.HF14) && w.BlockSigningKey == zero33) {
				selectedMiners[string(owner)] = true
			}
		}
		if uint8(len(selectedMiners)) >= ws.MaxMinerWitnesses {
			return false, nil
		}
		return true, nil
	})
	// Now mutate: each visited witness has pow_worker zeroed and
	// num_pow_witnesses decremented — matches C++ witness_schedule.cpp:194-204
	// (drops head of the by_pow index whether or not picked as miner).
	// Among visited, those flagged in selectedMiners get added to active
	// + Schedule = miner.
	for _, p := range powCandidates {
		w, err := objects.GetWitness(s, p.owner)
		if err != nil {
			continue
		}
		if selectedMiners[string(p.owner)] {
			active = append(active, picked{owner: w.Owner, kind: objects.WitnessScheduleMiner})
			w.Schedule = objects.WitnessScheduleMiner
		}
		if w.PowWorker != 0 {
			w.PowWorker = 0
			if dgpo.NumPowWitnesses > 0 {
				dgpo.NumPowWitnesses--
				if envGhivediffPowbump != "" {
					fmt.Fprintf(os.Stderr, "POWDRAIN block=%d num=%d\n", blockNum, dgpo.NumPowWitnesses)
				}
			}
		}
		objects.PutWitness(s, &w)
	}

	// 3) Fill remaining slots from by_schedule_time (timeshare).
	//    Iterate ascending VST. For each visited witness:
	//      - Advance new_virtual_time to their VST.
	//      - If they're not already elected/miner and not disabled, add
	//        them as a timeshare slot.
	//      - Stop when active size reaches HIVE_MAX_WITNESSES.
	//    Visited witnesses (whether picked or skipped) all get their
	//    VST re-rolled to current_virtual_time + LAP_LENGTH2 / (votes+1).
	//    On overflow, reset_virtual_schedule_time fires globally.
	currentVT := uint128FromBytes(ws.CurrentVirtualTime)
	newVT := new(big.Int).Set(currentVT)
	type processed struct {
		owner auth.AccountName
		vst   [16]byte
	}
	var visited []processed
	wantedTotal := int(ws.MaxVotedWitnesses) + int(ws.MaxMinerWitnesses) + int(ws.MaxRunnerWitnesses)
	if wantedTotal == 0 || wantedTotal > int(objects.MaxWitnesses) {
		wantedTotal = int(objects.MaxWitnesses)
	}
	// Two-pass: collect ALL by-schedule-time visits into `visited` first,
	// then iterate the slice to update Schedule + VirtualScheduledTime.
	// Stops once `active` reaches wantedTotal (counting added timeshares).
	tsPicked := 0
	_ = wstore.RangeByScheduleTime(func(w *objects.Witness) (bool, error) {
		if len(active)+tsPicked >= wantedTotal {
			return false, nil
		}
		owner := w.Owner
		visited = append(visited, processed{owner: owner, vst: w.VirtualScheduledTime})
			// Speculatively count this as a picked timeshare for the
			// stop condition — we'll discard the count if the witness
			// turns out to be already-elected or disabled in the
			// mutation pass.
			if !selectedVoted[string(owner)] && !selectedMiners[string(owner)] {
				tsPicked++
			}
			return true, nil
		})

	// First pass: walk visited in ASC order, advancing new_virtual_time
	// to each visited witness's OLD VST. After the loop, new_virtual_time
	// equals the LAST visited's old VST. Mark eligible visits as
	// timeshare. Mirrors C++ witness_schedule.cpp:243-260 exactly.
	for _, p := range visited {
		newVT.SetBytes(p.vst[:])
		w, err := objects.GetWitness(s, p.owner)
		if err != nil {
			continue
		}
		// Pick as timeshare if eligible.
		if !(hf.Has(hardfork.HF14) && w.BlockSigningKey == zero33) &&
			!selectedVoted[string(p.owner)] && !selectedMiners[string(p.owner)] {
			if len(active) < wantedTotal {
				active = append(active, picked{owner: w.Owner, kind: objects.WitnessScheduleTimeshare})
				w.Schedule = objects.WitnessScheduleTimeshare
				objects.PutWitness(s, &w)
			}
		}
	}

	// Second pass: re-roll each visited witness's VST using the SAME
	// new_virtual_time (= last visited's old VST). Mirrors C++
	// witness_schedule.cpp:265-275 exactly: every processed witness gets
	// virtual_position=0, virtual_last_update=new_virtual_time, and
	// virtual_scheduled_time=new_virtual_time + LAP/(votes+1). This is
	// THE source of subsequent timeshare ordering.
	resetVT := false
	lap2 := hiveVirtualScheduleLapLength2()
	for _, p := range visited {
		w, err := objects.GetWitness(s, p.owner)
		if err != nil {
			continue
		}
		votesPlus1 := big.NewInt(w.TotalVotesVests + 1)
		if votesPlus1.Sign() <= 0 {
			votesPlus1 = big.NewInt(1)
		}
		advance := new(big.Int).Quo(lap2, votesPlus1)
		newVST := new(big.Int).Add(newVT, advance)
		if newVST.Cmp(newVT) < 0 || newVST.BitLen() > 128 {
			resetVT = true
			break
		}
		w.VirtualPosition = [16]byte{}
		w.VirtualLastUpdate = uint128ToBytes(newVT)
		w.VirtualScheduledTime = uint128ToBytes(newVST)
		objects.PutWitness(s, &w)
	}
	if resetVT {
		if envGhivediffReset != "" {
			fmt.Fprintf(os.Stderr, "GHIVEDIFF RESET-VST block=%d\n", blockNum)
		}
		newVT.SetInt64(0)
		resetVirtualScheduleTime(s, ws)
	}

	// 4) Hardfork-version vote tally (post-HF5__54). The schedule's
	//    current_shuffled_witnesses is read by C++; we use the active
	//    set we just built (it's about to overwrite the schedule
	//    anyway). Iterate witnesses, tally running_version and
	//    (hardfork_version_vote, hardfork_time_vote). Pick the
	//    majority — the highest running_version with at least
	//    hardfork_required_witnesses votes — and bump the hardfork
	//    property object's next_hardfork to the most-voted (version,
	//    time) tuple.
	majorityVersion := ws.MajorityVersion
	if hf.Has(hardfork.HF5) {
		type verCount struct {
			version uint32
			count   uint32
		}
		var vers []verCount
		type hfvKey struct {
			version uint32
			t       uint32
		}
		hfvCounts := make(map[hfvKey]uint32)
		for _, p := range active {
			w, err := objects.GetWitness(s, p.owner)
			if err != nil {
				continue
			}
			found := false
			for i := range vers {
				if vers[i].version == w.RunningVersion {
					vers[i].count++
					found = true
					break
				}
			}
			if !found {
				vers = append(vers, verCount{version: w.RunningVersion, count: 1})
			}
			k := hfvKey{version: w.HardforkVersionVote, t: w.HardforkTimeVote}
			hfvCounts[k]++
		}
		// Sort vers by version DESC.
		sort.Slice(vers, func(i, j int) bool { return vers[i].version > vers[j].version })
		var sumCount uint32
		for _, v := range vers {
			sumCount += v.count
			if sumCount >= uint32(ws.HardforkRequiredWitnesses) {
				majorityVersion = v.version
				break
			}
		}
		// Hardfork-version vote bump: the first (version, time) tuple
		// with >= required votes wins. C++ iterates the map in
		// std::map order; map iteration in Go is random, so pick the
		// MIN tuple deterministically.
		type hfvEntry struct {
			version uint32
			t       uint32
			count   uint32
		}
		var hfvs []hfvEntry
		for k, c := range hfvCounts {
			hfvs = append(hfvs, hfvEntry{version: k.version, t: k.t, count: c})
		}
		sort.Slice(hfvs, func(i, j int) bool {
			if hfvs[i].version != hfvs[j].version {
				return hfvs[i].version < hfvs[j].version
			}
			return hfvs[i].t < hfvs[j].t
		})
		// Mirror C++ witness_schedule.cpp:289-348. Special-case HF22→HF23
		// where C++ forces the bump regardless of vote tally. Otherwise:
		// pick the first (version, time) tuple with >= required votes
		// (hfvs is sorted ascending; std::map order in C++); if no
		// majority, reset next_hardfork to current_hardfork_version.
		if hpo, hpoErr := objects.GetHardforkPropertyObject(s); hpoErr == nil {
			hf22Version := hardfork.PackedVersion(hardfork.HF22)
			hf23Version := hardfork.PackedVersion(hardfork.HF23)
			hf23Time := hardfork.MainnetActivationTime[hardfork.HF23]
			if hpo.CurrentHardforkVersion == hf22Version {
				if hpo.NextHardfork != hf23Version {
					hpo.NextHardfork = hf23Version
					hpo.NextHardforkTime = hf23Time
					objects.PutHardforkPropertyObject(s, &hpo)
				}
			} else {
				picked := false
				for _, e := range hfvs {
					if e.count >= uint32(ws.HardforkRequiredWitnesses) {
						if hpo.NextHardfork != e.version || hpo.NextHardforkTime != e.t {
							hpo.NextHardfork = e.version
							hpo.NextHardforkTime = e.t
							objects.PutHardforkPropertyObject(s, &hpo)
						}
						picked = true
						break
					}
				}
				if !picked {
					if hpo.NextHardfork != hpo.CurrentHardforkVersion {
						hpo.NextHardfork = hpo.CurrentHardforkVersion
						objects.PutHardforkPropertyObject(s, &hpo)
					}
				}
			}
		}
	}

	// 5) Write the active witnesses into the schedule, recompute pay
	//    normalization, and xorshift-shuffle.
	numElected := 0
	numMiner := 0
	numTimeshare := 0
	for _, p := range active {
		switch p.kind {
		case objects.WitnessScheduleElected:
			numElected++
		case objects.WitnessScheduleMiner:
			numMiner++
		case objects.WitnessScheduleTimeshare:
			numTimeshare++
		}
	}
	shuffled := make([]auth.AccountName, len(active))
	for i, p := range active {
		shuffled[i] = p.owner
	}
	xorshiftShuffleSchedule(shuffled, dgpo.Time)

	ws.CurrentShuffledWitnesses = shuffled
	if len(shuffled) > 0 {
		ws.NumScheduledWitnesses = uint8(len(shuffled))
	} else {
		ws.NumScheduledWitnesses = 1
	}
	ws.WitnessPayNormalizationFactor = ws.ElectedWeight*uint32(numElected) +
		ws.MinerWeight*uint32(numMiner) +
		ws.TimeshareWeight*uint32(numTimeshare)
	ws.CurrentVirtualTime = uint128ToBytes(newVT)
	ws.NextShuffleBlockNum = blockNum + uint32(ws.NumScheduledWitnesses)
	ws.MajorityVersion = majorityVersion

	updateMedianWitnessProps(s, ws)

	if ws.MedianHBDInterestRate > 0 {
		dgpo.HBDInterestRate = ws.MedianHBDInterestRate
	}
}

// updateWitnessSchedulePreHF4 ports the legacy update_witness_schedule
// path at libraries/chain/witness_schedule.cpp:441-569. The differences
// from update_witness_schedule_4:
//   - Lock out the top (HIVE_MAX_WITNESSES - 2) witnesses by setting their
//     VirtualScheduledTime to MAX so they're never picked as timeshare.
//   - Pick exactly ONE timeshare from by_schedule_time (skip pow workers).
//     Advance their VST by LAP_LENGTH (= 2^64-1) / (votes+1) pre-HF2 OR
//     LAP_LENGTH2 (= 2^128-1) / (votes+1) post-HF2.
//   - Pop ALL pow workers from the queue (not just one) up to
//     HIVE_MAX_WITNESSES total active.
func updateWitnessSchedulePreHF4(s *state.Session, ws *objects.WitnessSchedule, dgpo *objects.DynamicGlobalProperty, blockNum uint32, hf hardfork.Set) {
	zero33 := [33]byte{}
	var active []auth.AccountName
	newVT := new(big.Int)

	// Vote-based scheduling kicks in once 1M HIVE has been minted OR
	// no PoW miners are queued. Pre-201600 we relied on PoW only.
	const hiveStartMinerVotingBlock uint32 = 864_000
	useVoteBased := dgpo.NumPowWitnesses == 0 || blockNum > hiveStartMinerVotingBlock

	wstore := objects.EnsureWitnessStore(s)
	if useVoteBased {
		// Two-pass: collect by-vote candidates first, then mutate.
		// Lock out the top (HIVE_MAX_WITNESSES - 2) by setting VST = MAX.
		var byVoteCands []auth.AccountName
		_ = wstore.RangeByVote(func(w *objects.Witness) (bool, error) {
			if len(byVoteCands) >= int(objects.MaxWitnesses)-2 {
				return false, nil
			}
			byVoteCands = append(byVoteCands, w.Owner)
			return true, nil
		})
		picked := 0
		for _, owner := range byVoteCands {
			if picked >= int(objects.MaxWitnesses)-2 {
				break
			}
			w, err := objects.GetWitness(s, owner)
			if err != nil {
				continue
			}
			if w.PowWorker != 0 {
				continue
			}
			active = append(active, w.Owner)
			w.VirtualScheduledTime = uint128MaxBytes
			objects.PutWitness(s, &w)
			picked++
		}

		// Pick exactly one timeshare witness from by_schedule_time
		// (skip pow workers). Two-pass: first non-pow candidate.
		var tsCand auth.AccountName
		var tsCandFound bool
		_ = wstore.RangeByScheduleTime(func(w *objects.Witness) (bool, error) {
			if w.PowWorker != 0 {
				return true, nil
			}
			tsCand = w.Owner
			tsCandFound = true
			return false, nil
		})
		if tsCandFound {
			w, err := objects.GetWitness(s, tsCand)
			if err == nil {
				active = append(active, w.Owner)
				w.VirtualPosition = [16]byte{}
				newVT.SetBytes(w.VirtualScheduledTime[:])
				if newVT.Cmp(uint128Max()) == 0 {
					newVT.SetInt64(0)
				}
				votesPlus1 := big.NewInt(w.TotalVotesVests + 1)
				if votesPlus1.Sign() <= 0 {
					votesPlus1 = big.NewInt(1)
				}
				var lap *big.Int
				if hf.Has(hardfork.HF2) {
					lap = hiveVirtualScheduleLapLength2()
				} else {
					lap = hiveVirtualScheduleLapLength()
				}
				advance := new(big.Int).Quo(lap, votesPlus1)
				newVST := uint128FromBytes(w.VirtualScheduledTime)
				newVST.Add(newVST, advance)
				// C++ pre-HF4 (witness_schedule.cpp:520) does plain
				// `wo.virtual_scheduled_time += LAP/(votes+1)` in fc::uint128
				// which wraps mod 2^128. uint128ToBytes truncates to the
				// low 128 bits naturally, so just rely on that. Don't
				// reset to 0 on overflow (the previous code did, which
				// produced a different result than C++'s wrap).
				w.VirtualScheduledTime = uint128ToBytes(newVST)
				objects.PutWitness(s, &w)
				_ = zero33
			}
		}
	}

	// Drain the PoW queue. If num_pow_witnesses > HIVE_MAX_WITNESSES,
	// pop the lowest-PoW miner (zeroing their pow_worker + decrementing
	// num_pow_witnesses) before scheduling. Two-pass.
	if dgpo.NumPowWitnesses > uint32(objects.MaxWitnesses) {
		var firstPow auth.AccountName
		var firstPowFound bool
		_ = wstore.RangeByPow(func(w *objects.Witness) (bool, error) {
			if w.PowWorker == 0 {
				return true, nil
			}
			firstPow = w.Owner
			firstPowFound = true
			return false, nil
		})
		if firstPowFound {
			w, err := objects.GetWitness(s, firstPow)
			if err == nil && w.PowWorker != 0 {
				w.PowWorker = 0
				objects.PutWitness(s, &w)
				if dgpo.NumPowWitnesses > 0 {
					dgpo.NumPowWitnesses--
				if envGhivediffPowbump != "" {
					fmt.Fprintf(os.Stderr, "POWDRAIN block=%d num=%d\n", blockNum, dgpo.NumPowWitnesses)
				}
				}
			}
		}
	}

	// Add PoW miners until we hit HIVE_MAX_WITNESSES OR (post-START_MINER_VOTING)
	// just one PoW miner per round. Two-pass: collect, then check stop.
	var powOwners []auth.AccountName
	_ = wstore.RangeByPow(func(w *objects.Witness) (bool, error) {
		if w.PowWorker == 0 {
			return true, nil
		}
		powOwners = append(powOwners, w.Owner)
		if len(powOwners) >= int(objects.MaxWitnesses) {
			return false, nil
		}
		return true, nil
	})
	for _, owner := range powOwners {
		active = append(active, owner)
		if blockNum > hiveStartMinerVotingBlock || len(active) >= int(objects.MaxWitnesses) {
			break
		}
	}

	// Cap to MaxWitnesses (active may have come over).
	if len(active) > int(objects.MaxWitnesses) {
		active = active[:int(objects.MaxWitnesses)]
	}

	xorshiftShuffleSchedule(active, dgpo.Time)
	ws.CurrentShuffledWitnesses = active
	if len(active) > 0 {
		ws.NumScheduledWitnesses = uint8(len(active))
	} else {
		ws.NumScheduledWitnesses = 1
	}
	if useVoteBased {
		ws.CurrentVirtualTime = uint128ToBytes(newVT)
	}
	ws.NextShuffleBlockNum = blockNum + uint32(ws.NumScheduledWitnesses)
	ws.WitnessPayNormalizationFactor = uint32(len(active))
	updateMedianWitnessProps(s, ws)
}

// resetVirtualScheduleTime mirrors witness_schedule.cpp:21-40. Sets
// the chain-wide current_virtual_time to 0 and re-seeds every
// witness's VST to LAP_LENGTH2 / (votes+1) (with virtual_position
// reset to 0 and virtual_last_update to 0).
func resetVirtualScheduleTime(s *state.Session, ws *objects.WitnessSchedule) {
	ws.CurrentVirtualTime = [16]byte{}
	type entry struct {
		owner auth.AccountName
	}
	var owners []entry
	_ = s.Range(objects.WitnessTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			w, err := objects.ReadWitness(value)
			if err != nil {
				return true, nil
			}
			owners = append(owners, entry{owner: w.Owner})
			return true, nil
		})
	lap2 := hiveVirtualScheduleLapLength2()
	zeroVT := new(big.Int)
	for _, e := range owners {
		w, err := objects.GetWitness(s, e.owner)
		if err != nil {
			continue
		}
		w.VirtualPosition = [16]byte{}
		w.VirtualLastUpdate = uint128ToBytes(zeroVT)
		votesPlus1 := big.NewInt(w.TotalVotesVests + 1)
		if votesPlus1.Sign() <= 0 {
			votesPlus1 = big.NewInt(1)
		}
		w.VirtualScheduledTime = uint128ToBytes(new(big.Int).Quo(lap2, votesPlus1))
		objects.PutWitness(s, &w)
	}
}

// xorshiftShuffleSchedule applies hived's per-block deterministic shuffle
// over the active set (witness_schedule.cpp:373-388). Seeded by
// head_block_time << 32 + i*KNUTH_C, then 3 xorshifts and KNUTH_C.
func xorshiftShuffleSchedule(witnesses []auth.AccountName, headBlockTime uint32) {
	const knuthC uint64 = 2685821657736338717
	nowHi := uint64(headBlockTime) << 32
	for i := uint32(0); i < uint32(len(witnesses)); i++ {
		k := nowHi + uint64(i)*knuthC
		k ^= k >> 12
		k ^= k << 25
		k ^= k >> 27
		k *= knuthC
		jmax := uint32(len(witnesses)) - i
		j := i + uint32(k%uint64(jmax))
		witnesses[i], witnesses[j] = witnesses[j], witnesses[i]
	}
}

// updateMedianWitnessProps mirrors witness_schedule.cpp:58. Sorts the
// active set by each witness-property field and picks the median.
func updateMedianWitnessProps(s *state.Session, ws *objects.WitnessSchedule) {
	if len(ws.CurrentShuffledWitnesses) == 0 {
		return
	}
	type props struct {
		acf  int64
		size uint32
		rate uint16
	}
	all := make([]props, 0, len(ws.CurrentShuffledWitnesses))
	for i := uint8(0); i < ws.NumScheduledWitnesses && int(i) < len(ws.CurrentShuffledWitnesses); i++ {
		w, err := objects.GetWitness(s, ws.CurrentShuffledWitnesses[i])
		if err != nil {
			continue
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

// decodeBE64 reads 8 big-endian bytes as a uint64.
func decodeBE64(b []byte) uint64 {
	var v uint64
	for i := 0; i < 8 && i < len(b); i++ {
		v = v<<8 | uint64(b[i])
	}
	return v
}
