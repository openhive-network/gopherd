package chain

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"os"
	"sort"

	"hived-go/internal/math128"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

func bigEndianUint64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }

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
// witnessScheduleScratch is the per-Database scratch buffer for
// updateWitnessSchedule4's per-round work. ApplyBlock holds applyMu,
// so reuse across rounds is single-threaded and safe.
type witnessScheduleScratch struct {
	active          []schedPicked
	voteCands       []auth.AccountName
	powCandidates   []schedPowEntry
	visited         []schedProcessed
	selectedVoted   map[string]bool
	selectedMiners  map[string]bool
	miners          []schedPicked // helper used during sort/version-tally
}

type schedPicked struct {
	owner auth.AccountName
	kind  objects.WitnessScheduleType
}

type schedPowEntry struct {
	owner auth.AccountName
	pow   uint64
}

type schedProcessed struct {
	owner auth.AccountName
	vst   [16]byte
}

func (s *witnessScheduleScratch) reset() {
	s.active = s.active[:0]
	s.voteCands = s.voteCands[:0]
	s.powCandidates = s.powCandidates[:0]
	s.visited = s.visited[:0]
	if s.selectedVoted == nil {
		s.selectedVoted = make(map[string]bool, 32)
	} else {
		clear(s.selectedVoted)
	}
	if s.selectedMiners == nil {
		s.selectedMiners = make(map[string]bool, 8)
	} else {
		clear(s.selectedMiners)
	}
	s.miners = s.miners[:0]
}

func updateWitnessSchedule(s *state.Session, blockNum uint32, scratch *witnessScheduleScratch) {
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
			maxWit := int(objects.MaxWitnesses)
			if len(ws.RecentBlockProducers) < maxWit {
				ws.RecentBlockProducers = append(ws.RecentBlockProducers, newProd)
			} else {
				// In-place shift left + append. Reuses the backing array
				// (cap=MaxWitnesses) — the re-slice path that previously
				// did `ws[len-MaxWitnesses:]` left len=cap, so the next
				// append always heap-allocated a new array. ~120 MB cum.
				copy(ws.RecentBlockProducers, ws.RecentBlockProducers[1:])
				ws.RecentBlockProducers[maxWit-1] = newProd
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
		scratch.reset()
		updateWitnessSchedule4(s, &ws, &dgpo, blockNum, hf, scratch)
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
func updateWitnessSchedule4(s *state.Session, ws *objects.WitnessSchedule, dgpo *objects.DynamicGlobalProperty, blockNum uint32, hf hardfork.Set, scratch *witnessScheduleScratch) {
	zero33 := [33]byte{}

	active := scratch.active
	selectedVoted := scratch.selectedVoted
	voteCands := scratch.voteCands

	// 1) Top max_voted by votes (skipping disabled witnesses post-HF14).
	//    Two-pass: collect candidates from the by-vote index, then mutate
	//    Schedule + persist outside the iteration so we don't mutate the
	//    index we're scanning.
	//
	//    C++ witness_schedule.cpp:162 iterates by_vote_name, SKIPPING
	//    (not stopping at) HF14+ disabled witnesses, until it has
	//    accumulated max_voted_witnesses non-disabled. The candidate
	//    pool size must therefore be unbounded — capping at
	//    HIVE_MAX_WITNESSES (21) before filtering would silently drop
	//    lower-ranked enabled witnesses if any of the top-21 are
	//    disabled, leaking votes out of the active set.
	wstore := objects.EnsureWitnessStore(s)
	{
		selected := 0
		_ = wstore.RangeByVote(func(w *objects.Witness) (bool, error) {
			if selected >= int(ws.MaxVotedWitnesses) {
				return false, nil
			}
			if hf.Has(hardfork.HF14) && w.BlockSigningKey == zero33 {
				return true, nil
			}
			voteCands = append(voteCands, w.Owner)
			selected++
			return true, nil
		})
	}
	for _, owner := range voteCands {
		w, err := objects.GetWitness(s, owner)
		if err != nil {
			continue
		}
		selectedVoted[string(w.Owner)] = true
		active = append(active, schedPicked{owner: w.Owner, kind: objects.WitnessScheduleElected})
		w.Schedule = objects.WitnessScheduleElected
		objects.PutWitness(s, &w)
	}

	// Comprehensive per-rebuild full-state dump matching hived's
	// placement (right after voted iter, before miner + timeshare passes).
	// Use the cached env value to avoid the per-rebuild os.Getenv syscall.
	if envGhivediffDumpFile != "" {
		dumpFullWitnessTSV(s, blockNum, ws, envGhivediffDumpFile)
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
	selectedMiners := scratch.selectedMiners
	powCandidates := scratch.powCandidates
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
		powCandidates = append(powCandidates, schedPowEntry{owner: owner, pow: w.PowWorker})
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
			active = append(active, schedPicked{owner: w.Owner, kind: objects.WitnessScheduleMiner})
			w.Schedule = objects.WitnessScheduleMiner
		}
		if w.PowWorker != 0 {
			w.PowWorker = 0
			if dgpo.NumPowWitnesses > 0 {
				dgpo.NumPowWitnesses--
				if envGhivediffPowbump != "" {
					fmt.Fprintf(os.Stderr, "POWDRAIN block=%d num=%d witness=%s\n", blockNum, dgpo.NumPowWitnesses, w.Owner)
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
	visited := scratch.visited
	wantedTotal := int(ws.MaxVotedWitnesses) + int(ws.MaxMinerWitnesses) + int(ws.MaxRunnerWitnesses)
	if wantedTotal == 0 || wantedTotal > int(objects.MaxWitnesses) {
		wantedTotal = int(objects.MaxWitnesses)
	}
	// Two-pass: collect ALL by-schedule-time visits into `visited` first,
	// then iterate the slice to update Schedule + VirtualScheduledTime.
	// Stops once `active` reaches wantedTotal (counting added timeshares).
	//
	// C++ witness_schedule.cpp:323 only ++witness_count for visits that
	// (a) are not HF14+ disabled, AND (b) are not already in
	// selected_voted/selected_miners. Disabled witnesses are still
	// pushed to processed_witnesses (so their VST is re-rolled), but
	// don't count toward the stop threshold. Match exactly: incrementing
	// tsPicked for a disabled witness would leak an active slot.
	tsPicked := 0
	_ = wstore.RangeByScheduleTime(func(w *objects.Witness) (bool, error) {
		if len(active)+tsPicked >= wantedTotal {
			return false, nil
		}
		owner := w.Owner
		visited = append(visited, schedProcessed{owner: owner, vst: w.VirtualScheduledTime})
		if !selectedVoted[string(owner)] && !selectedMiners[string(owner)] &&
			!(hf.Has(hardfork.HF14) && w.BlockSigningKey == zero33) {
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
				active = append(active, schedPicked{owner: w.Owner, kind: objects.WitnessScheduleTimeshare})
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
	//
	// Hot inner loop: math128.U128 replaces a math/big triple-alloc per
	// witness (big.NewInt(votes+1), big.Int.Quo, big.Int.Add). lap2 here
	// is the constant 2^128-1; advance = lap2/(votes+1) fits in U128;
	// newVST = newVT + advance, with overflow tracked via AddOverflow's
	// carry bit (mirrors the bit-128 check the math/big path used).
	resetVT := false
	newVTBytes := uint128ToBytes(newVT)
	newVTU := math128.U128{
		Hi: bigEndianUint64(newVTBytes[0:8]),
		Lo: bigEndianUint64(newVTBytes[8:16]),
	}
	lap2U := math128.U128{Hi: ^uint64(0), Lo: ^uint64(0)}
	for _, p := range visited {
		w, err := objects.GetWitness(s, p.owner)
		if err != nil {
			continue
		}
		vp1 := uint64(w.TotalVotesVests + 1)
		if w.TotalVotesVests+1 <= 0 {
			vp1 = 1
		}
		advance := lap2U.DivU64(vp1)
		newVSTU, carry := newVTU.AddOverflow(advance)
		if carry != 0 {
			resetVT = true
			break
		}
		w.VirtualPosition = [16]byte{}
		w.VirtualLastUpdate = newVTBytes
		w.VirtualScheduledTime = [16]byte{
			byte(newVSTU.Hi >> 56), byte(newVSTU.Hi >> 48), byte(newVSTU.Hi >> 40), byte(newVSTU.Hi >> 32),
			byte(newVSTU.Hi >> 24), byte(newVSTU.Hi >> 16), byte(newVSTU.Hi >> 8), byte(newVSTU.Hi),
			byte(newVSTU.Lo >> 56), byte(newVSTU.Lo >> 48), byte(newVSTU.Lo >> 40), byte(newVSTU.Lo >> 32),
			byte(newVSTU.Lo >> 24), byte(newVSTU.Lo >> 16), byte(newVSTU.Lo >> 8), byte(newVSTU.Lo),
		}
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

	// Persist grown capacities back into the scratch — next round
	// resets to len=0 but keeps cap.
	scratch.active = active
	scratch.voteCands = voteCands
	scratch.powCandidates = powCandidates
	scratch.visited = visited
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
		// C++ witness_schedule.cpp:560 iterates by_vote_name, SKIPPING
		// (not stopping at) pow_workers, until it has accumulated
		// HIVE_MAX_WITNESSES-2 = 19 non-pow witnesses. The Go bug
		// fixed here was capping the candidate pool at 19 BEFORE
		// filtering pow_workers, so any pow-worker in the top-19
		// vote slots silently shrank the schedule and leaked actual
		// vote-leaders out of the active set (e.g., boatymcboatface,
		// joseph at block 933,600).
		var byVoteCands []auth.AccountName
		selected := 0
		_ = wstore.RangeByVote(func(w *objects.Witness) (bool, error) {
			if selected >= int(objects.MaxWitnesses)-2 {
				return false, nil
			}
			if w.PowWorker != 0 {
				return true, nil
			}
			byVoteCands = append(byVoteCands, w.Owner)
			selected++
			return true, nil
		})
		for _, owner := range byVoteCands {
			w, err := objects.GetWitness(s, owner)
			if err != nil {
				continue
			}
			active = append(active, w.Owner)
			w.VirtualScheduledTime = uint128MaxBytes
			objects.PutWitness(s, &w)
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
					fmt.Fprintf(os.Stderr, "POWDRAIN block=%d num=%d witness=%s\n", blockNum, dgpo.NumPowWitnesses, w.Owner)
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
	_ = objects.EnsureWitnessStore(s).RangePrimary(func(w *objects.Witness) (bool, error) {
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
	// N ≤ HIVE_MAX_WITNESSES = 21. Use three parallel arrays sorted via
	// insertion sort (fastest for N≤21 and avoids the reflectlite.Swapper
	// alloc that sort.Slice triggered three times per call).
	// GetWitnessRef returns a read-only pointer — skips three ~hundred-
	// byte Witness value copies per witness.
	var (
		acf  [21]int64
		size [21]uint32
		rate [21]uint16
	)
	n := 0
	limit := int(ws.NumScheduledWitnesses)
	if limit > len(ws.CurrentShuffledWitnesses) {
		limit = len(ws.CurrentShuffledWitnesses)
	}
	if limit > 21 {
		limit = 21
	}
	for i := 0; i < limit; i++ {
		w, ok := objects.GetWitnessRef(s, ws.CurrentShuffledWitnesses[i])
		if !ok {
			continue
		}
		acf[n] = w.AccountCreationFee
		size[n] = w.MaximumBlockSize
		rate[n] = w.HBDInterestRate
		n++
	}
	if n == 0 {
		return
	}
	// Insertion sort each lane independently.
	for i := 1; i < n; i++ {
		x := acf[i]
		j := i
		for ; j > 0 && acf[j-1] > x; j-- {
			acf[j] = acf[j-1]
		}
		acf[j] = x
	}
	for i := 1; i < n; i++ {
		x := size[i]
		j := i
		for ; j > 0 && size[j-1] > x; j-- {
			size[j] = size[j-1]
		}
		size[j] = x
	}
	for i := 1; i < n; i++ {
		x := rate[i]
		j := i
		for ; j > 0 && rate[j-1] > x; j-- {
			rate[j] = rate[j-1]
		}
		rate[j] = x
	}
	ws.MedianAccountCreationFee = acf[n/2]
	ws.MedianMaximumBlockSize = size[n/2]
	ws.MedianHBDInterestRate = rate[n/2]
}

// decodeBE64 reads 8 big-endian bytes as a uint64.
func decodeBE64(b []byte) uint64 {
	var v uint64
	for i := 0; i < 8 && i < len(b); i++ {
		v = v<<8 | uint64(b[i])
	}
	return v
}
