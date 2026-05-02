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
	// last). C++'s update_median_feed iterates the wso schedule which
	// is rebuilt each round from max_voted/max_miner/timeshare slots —
	// without modeling the miner queue or virtual-time fairness queue
	// we can't compute the schedule the same way; instead we track who
	// actually produced the recent blocks (the producer name comes
	// from the trusted block header). The set of producers in this
	// window matches C++'s active schedule by definition since the
	// chain ran on C++'s schedule.
	RecentBlockProducers []auth.AccountName
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

// PutWitnessSchedule updates the per-Session lazy cache. Serialization
// to the overlay is deferred to commit time via a pre-commit hook;
// hooks that re-Put a structurally-identical schedule (the early-return
// path in update_witness_schedule) only mutate the cache and the
// schedule isn't re-encoded at all if no other Put fires.
func PutWitnessSchedule(s *state.Session, w *WitnessSchedule) {
	if s.Aux == nil {
		s.Aux = make(map[string]any, 4)
	}
	cached, ok := s.Aux[witnessScheduleLazyKey].(*witnessScheduleLazy)
	if !ok {
		cached = &witnessScheduleLazy{val: *w}
		s.Aux[witnessScheduleLazyKey] = cached
	} else {
		cached.val = *w
	}
	if !cached.dirty {
		cached.dirty = true
		s.RegisterPreCommit(func() {
			s.SerScratch = AppendWitnessSchedule(s.SerScratch[:0], &cached.val)
			s.Put(WitnessScheduleTable, state.PrimaryIndex, []byte{0}, s.SerScratch)
			s.DatabaseAuxInvalidate(witnessScheduleAuxKey)
		})
	}
}

// GetWitnessSchedule fetches the singleton. Within a block the per-
// Session lazy cache returns the in-progress mutated struct; across
// blocks the cross-Session Aux cache returns the parsed struct loaded
// from the backend.
func GetWitnessSchedule(s *state.Session) (WitnessSchedule, error) {
	if s.Aux != nil {
		if lazy, ok := s.Aux[witnessScheduleLazyKey].(*witnessScheduleLazy); ok {
			return lazy.val, nil
		}
	}
	if !s.HasOverlayWrite(WitnessScheduleTable, state.PrimaryIndex) {
		if cached, ok := s.DatabaseAux(witnessScheduleAuxKey); ok {
			if w, ok := cached.(WitnessSchedule); ok {
				return w, nil
			}
		}
	}
	v, err := s.Get(WitnessScheduleTable, state.PrimaryIndex, []byte{0})
	if err != nil {
		return WitnessSchedule{}, err
	}
	w, err := ReadWitnessSchedule(v)
	if err != nil {
		return WitnessSchedule{}, err
	}
	s.DatabaseAuxStore(witnessScheduleAuxKey, w)
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
