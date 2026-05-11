package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// WitnessScheduleTable is the singleton table for the active witness
// schedule. Mirrors hive::chain::witness_schedule_object
// (libraries/chain/include/hive/chain/detail/state/witness_objects.hpp:183).
const WitnessScheduleTable state.TableID = 21

// MaxWitnesses — HIVE_MAX_WITNESSES (libraries/protocol/include/hive/
// protocol/config.hpp). Hard-coded at 21.
const MaxWitnesses = 21

// MaxVotedWitnesses, MaxMinerWitnesses — pre-HF20 splits the 21 slots
// into 19 elected + 1 miner + 1 timeshare. HF20+ goes to 20 elected
// + 1 timeshare (no miners). For our minimum schedule we follow the
// HF20 split since most tests run at HF17+.
const (
	MaxVotedWitnessesHF0  uint8 = 19
	MaxMinerWitnessesHF0  uint8 = 1
	MaxVotedWitnessesHF20 uint8 = 20
)

// WitnessSchedule tracks the 21-slot rotation, virtual-time markers,
// and median chain props sourced from the active set.
//
// We keep CurrentShuffledWitnesses as a length-prefixed slice rather
// than a fixed-size array (hived uses fc::array<account_name_type, 21>)
// — the slice can be shorter than 21 in the early chain when fewer
// witnesses are registered.
type WitnessSchedule struct {
	CurrentVirtualTime [16]byte
	NextShuffleBlockNum uint32
	CurrentShuffledWitnesses []auth.AccountName
	NumScheduledWitnesses    uint8

	// Median props pulled from the current shuffled set.
	MedianAccountCreationFee int64
	MedianMaximumBlockSize   uint32
	MedianHBDInterestRate    uint16

	MaxVotedWitnesses uint8
	MaxMinerWitnesses uint8

	// HF16+ pay-weighting: each scheduled slot type has a weight,
	// and process_funds distributes the witness_reward pot via
	// witness_reward *= 21 * type_weight / pay_normalization.
	// Defaults from witness_schedule_object: timeshare=5, miner=1,
	// elected=1; normalization = sum(num_T*W_T + num_M*W_M + num_E*W_E).
	TimeshareWeight             uint32
	MinerWeight                 uint32
	ElectedWeight               uint32
	WitnessPayNormalizationFactor uint32

	// RecentBlockProducers is a sliding window of producer names from
	// the most recent HIVE_MAX_WITNESSES blocks (oldest first, newest
	// last). Kept around historically when we hadn't yet ported
	// virtual_scheduled_time scheduling — the genuine schedule comes
	// from update_witness_schedule_4 now. Retained on disk for legacy
	// rows; new rows leave it empty.
	RecentBlockProducers []auth.AccountName
	// MajorityVersion is the highest version the schedule's witnesses
	// have running on (sum across witnesses must reach
	// HardforkRequiredWitnesses for the version to count). Mirrors
	// witness_schedule_object::majority_version.
	MajorityVersion uint32
	// HardforkRequiredWitnesses defaults to HIVE_HARDFORK_REQUIRED_WITNESSES
	// (17 of 21). Read by update_witness_schedule_4 when tallying
	// version votes.
	HardforkRequiredWitnesses uint8
	// MaxRunnerWitnesses (a.k.a. timeshare slots count). Pre-HF20: 1;
	// HF17+: 1. Together with MaxVotedWitnesses + MaxMinerWitnesses
	// must sum to HIVE_MAX_WITNESSES (21).
	MaxRunnerWitnesses uint8
}

// AppendWitnessSchedule serializes the schedule.
func AppendWitnessSchedule(dst []byte, w *WitnessSchedule) []byte {
	dst = append(dst, w.CurrentVirtualTime[:]...)
	dst = appendU32LE(dst, w.NextShuffleBlockNum)
	dst = fcraw.AppendVarUint(dst, uint32(len(w.CurrentShuffledWitnesses)))
	for _, n := range w.CurrentShuffledWitnesses {
		dst = fcraw.AppendVarUint(dst, uint32(len(n)))
		dst = append(dst, n...)
	}
	dst = append(dst, w.NumScheduledWitnesses)
	dst = appendI64LE(dst, w.MedianAccountCreationFee)
	dst = appendU32LE(dst, w.MedianMaximumBlockSize)
	dst = append(dst, byte(w.MedianHBDInterestRate), byte(w.MedianHBDInterestRate>>8))
	dst = append(dst, w.MaxVotedWitnesses, w.MaxMinerWitnesses)
	dst = appendU32LE(dst, w.TimeshareWeight)
	dst = appendU32LE(dst, w.MinerWeight)
	dst = appendU32LE(dst, w.ElectedWeight)
	dst = appendU32LE(dst, w.WitnessPayNormalizationFactor)
	dst = fcraw.AppendVarUint(dst, uint32(len(w.RecentBlockProducers)))
	for _, n := range w.RecentBlockProducers {
		dst = fcraw.AppendVarUint(dst, uint32(len(n)))
		dst = append(dst, n...)
	}
	dst = appendU32LE(dst, w.MajorityVersion)
	dst = append(dst, w.HardforkRequiredWitnesses, w.MaxRunnerWitnesses)
	return dst
}

// ReadWitnessSchedule decodes a schedule.
func ReadWitnessSchedule(src []byte) (WitnessSchedule, error) {
	r := fcraw.NewReader(src)
	var w WitnessSchedule
	if err := r.ReadInto(w.CurrentVirtualTime[:]); err != nil {
		return w, fmt.Errorf("ws.current_virtual_time: %w", err)
	}
	var err error
	if w.NextShuffleBlockNum, err = r.ReadUint32(); err != nil {
		return w, fmt.Errorf("ws.next_shuffle_block_num: %w", err)
	}
	count, err := r.ReadVarUint()
	if err != nil {
		return w, fmt.Errorf("ws.shuffled.len: %w", err)
	}
	if count > 0 {
		w.CurrentShuffledWitnesses = make([]auth.AccountName, count)
		for i := uint32(0); i < count; i++ {
			s, err := r.ReadString()
			if err != nil {
				return w, fmt.Errorf("ws.shuffled[%d]: %w", i, err)
			}
			w.CurrentShuffledWitnesses[i] = s
		}
	}
	if w.NumScheduledWitnesses, err = r.ReadByte(); err != nil {
		return w, fmt.Errorf("ws.num_scheduled: %w", err)
	}
	if w.MedianAccountCreationFee, err = r.ReadInt64(); err != nil {
		return w, fmt.Errorf("ws.median_acf: %w", err)
	}
	if w.MedianMaximumBlockSize, err = r.ReadUint32(); err != nil {
		return w, fmt.Errorf("ws.median_block_size: %w", err)
	}
	if w.MedianHBDInterestRate, err = r.ReadUint16(); err != nil {
		return w, fmt.Errorf("ws.median_hbd_rate: %w", err)
	}
	if w.MaxVotedWitnesses, err = r.ReadByte(); err != nil {
		return w, fmt.Errorf("ws.max_voted: %w", err)
	}
	if w.MaxMinerWitnesses, err = r.ReadByte(); err != nil {
		return w, fmt.Errorf("ws.max_miner: %w", err)
	}
	// Per-slot pay weights are a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return w, nil
	}
	if w.TimeshareWeight, err = r.ReadUint32(); err != nil {
		return w, fmt.Errorf("ws.timeshare_weight: %w", err)
	}
	if w.MinerWeight, err = r.ReadUint32(); err != nil {
		return w, fmt.Errorf("ws.miner_weight: %w", err)
	}
	if w.ElectedWeight, err = r.ReadUint32(); err != nil {
		return w, fmt.Errorf("ws.elected_weight: %w", err)
	}
	if w.WitnessPayNormalizationFactor, err = r.ReadUint32(); err != nil {
		return w, fmt.Errorf("ws.pay_normalization: %w", err)
	}
	// Recent block producers — schema addition; tolerate EOF for older
	// rows that pre-date this field.
	if r.AtEnd() {
		return w, nil
	}
	rcount, err := r.ReadVarUint()
	if err != nil {
		return w, fmt.Errorf("ws.recent_producers.len: %w", err)
	}
	if rcount > 0 {
		w.RecentBlockProducers = make([]auth.AccountName, rcount)
		for i := uint32(0); i < rcount; i++ {
			s, err := r.ReadString()
			if err != nil {
				return w, fmt.Errorf("ws.recent_producers[%d]: %w", i, err)
			}
			w.RecentBlockProducers[i] = s
		}
	}
	if r.AtEnd() {
		return w, nil
	}
	if w.MajorityVersion, err = r.ReadUint32(); err != nil {
		return w, fmt.Errorf("ws.majority_version: %w", err)
	}
	if w.HardforkRequiredWitnesses, err = r.ReadByte(); err != nil {
		return w, fmt.Errorf("ws.hardfork_required_witnesses: %w", err)
	}
	if w.MaxRunnerWitnesses, err = r.ReadByte(); err != nil {
		return w, fmt.Errorf("ws.max_runner_witnesses: %w", err)
	}
	if !r.AtEnd() {
		return w, errors.New("witness_schedule: trailing bytes")
	}
	return w, nil
}

const witnessScheduleAuxKey = "witness_schedule"
const witnessScheduleLazyKey = "witness_schedule_lazy"

type witnessScheduleLazy struct {
	val   WitnessSchedule
	dirty bool
}

// PutWitnessSchedule writes via the typed WitnessScheduleStore. No
// K/V Put — the previous serialize+overlay path was the second-largest
// Session.Put alloc source (~1.84 GB / 4M blocks for once-per-block
// schedule updates). Snapshot/Restore handled by the subsystem hooks.
func PutWitnessSchedule(s *state.Session, w *WitnessSchedule) {
	EnsureWitnessScheduleStore(s).Put(w)
}

// GetWitnessSchedule fetches the singleton.
func GetWitnessSchedule(s *state.Session) (WitnessSchedule, error) {
	w, ok := EnsureWitnessScheduleStore(s).Get()
	if !ok {
		return WitnessSchedule{}, state.ErrNotFound
	}
	return w, nil
}

// _unused_GetWitnessScheduleKV is kept as a placeholder for the prior
// implementation so the comment-cache invariants doc below still
// references something meaningful.
func _unused_GetWitnessScheduleKV(s *state.Session) (WitnessSchedule, error) {
	v, err := s.Get(WitnessScheduleTable, state.PrimaryIndex, []byte{0})
	if err != nil {
		return WitnessSchedule{}, err
	}
	w, err := ReadWitnessSchedule(v)
	if err != nil {
		return WitnessSchedule{}, err
	}
	// Only cache backend reads. An overlay-derived value can be Undone
	// later, leaving the cross-Session cache pointing at an abandoned
	// state.
	if !s.HasOverlayWrite(WitnessScheduleTable, state.PrimaryIndex) {
		s.DatabaseAuxStore(witnessScheduleAuxKey, w)
	}
	return w, nil
}

// HasWitnessSchedule reports existence.
func HasWitnessSchedule(s *state.Session) bool {
	has, _ := s.Has(WitnessScheduleTable, state.PrimaryIndex, []byte{0})
	return has
}

// ScheduledWitnessForSlot returns the witness who should produce the
// block at slot_num counting from 1 (1 = next block, 2 = block after,
// etc.). Mirrors hived's database::get_scheduled_witness.
//
// slot_num=1 returns the next producer; the formula is
// shuffled[(head_block_num + slot_num) % num_scheduled].
func (w *WitnessSchedule) ScheduledWitnessForSlot(headBlockNum, slotNum uint32) auth.AccountName {
	if w.NumScheduledWitnesses == 0 || len(w.CurrentShuffledWitnesses) == 0 {
		return ""
	}
	idx := (headBlockNum + slotNum) % uint32(w.NumScheduledWitnesses)
	if int(idx) >= len(w.CurrentShuffledWitnesses) {
		return ""
	}
	return w.CurrentShuffledWitnesses[idx]
}

// ScheduledWitnessForSlotAslot is the corrected variant that mirrors
// hived's `get_scheduled_witness( slot_num )` =
// `current_shuffled_witnesses[(current_aslot + slot_num) % num_scheduled]`.
// `current_aslot` advances per slot — INCLUDING missed slots — whereas
// head_block_num advances only on produced blocks. Use this in
// per-block code that resolves the producer for a given slot
// (update_witness_missed_blocks) so missed-slot attribution mirrors
// C++. Without this fix, gxt-1080-sc-0028 saw 1 missed slot in Go but
// 2 in hived at block 5,140,000 — the second one would have triggered
// the HF14+ disable check, which is why hived's signing_key was zero
// (DISABLED) and Go's wasn't.
func (w *WitnessSchedule) ScheduledWitnessForSlotAslot(currentAslot uint64, slotNum uint32) auth.AccountName {
	if w.NumScheduledWitnesses == 0 || len(w.CurrentShuffledWitnesses) == 0 {
		return ""
	}
	idx := (currentAslot + uint64(slotNum)) % uint64(w.NumScheduledWitnesses)
	if int(idx) >= len(w.CurrentShuffledWitnesses) {
		return ""
	}
	return w.CurrentShuffledWitnesses[idx]
}
