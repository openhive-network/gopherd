package objects

import (
	"errors"
	"fmt"

	"gopherd/internal/fcraw"
	"gopherd/internal/state"
)

// HardforkPropertyTable is the singleton table for hardfork_property_object.
// Mirrors hive::chain::hardfork_property_object
// (libraries/chain/include/hive/chain/detail/state/hardfork_property_object.hpp).
const HardforkPropertyTable state.TableID = 24

var hardforkPropertySingletonKey = []byte{0}

// HardforkPropertyObject tracks which hardforks have been processed and
// the witness-vote-driven next_hardfork target. C++ has 28 named hardforks
// (HF0..HF28); processed_hardforks is a vector of activation timestamps
// where index 0 is genesis time and index N is the head_block_time when
// HFN fired. last_hardfork is N (the highest applied HF). Witnesses vote
// for a (next_hardfork_version, next_hardfork_time) pair via header
// extensions; update_witness_schedule_4 tallies votes from the active
// schedule and bumps the next_hardfork fields once
// HardforkRequiredWitnesses (17 of 21) align.
//
// Replay activates hardforks by block number — the witness-vote consensus
// is consensus-cosmetic for replay since the chain has already validated
// every hardfork transition. We still maintain the object faithfully for
// parity: process_block_header asserts that the producing witness's
// running_version >= current_hardfork_version, so once we wire signing
// validation we'll need an authoritative current_hardfork_version.
type HardforkPropertyObject struct {
	// ProcessedHardforks is a 0-indexed slice of activation timestamps.
	// ProcessedHardforks[0] = genesis time; ProcessedHardforks[i] for
	// i > 0 is the head_block_time when HFi was applied. Length is
	// LastHardfork+1.
	ProcessedHardforks []uint32
	LastHardfork       uint32
	// CurrentHardforkVersion is the packed v_num (major<<24 | minor<<16)
	// of the highest applied hardfork. Pre-HF1: 0x00000000.
	CurrentHardforkVersion uint32
	// NextHardfork is the packed v_num that the active schedule's
	// witnesses are voting for. NextHardforkTime is the time_point_sec
	// at which the vote window opens. Both are bumped whenever
	// >= HardforkRequiredWitnesses agree on a (version, time) tuple.
	NextHardfork     uint32
	NextHardforkTime uint32
	// DynamicActivations records, for hardforks that had no activation block
	// in gopherd's table when they fired, the mainnet block at which the
	// witness vote activated them (chain.processHardforks). gopherd has no
	// hived counterpart for this field: hived gates everything on
	// last_hardfork, gopherd on a per-block table that must learn the block.
	// It lives in state so an undo (fork switch) or a restart re-derives the
	// table from it. Sorted by ID; absent in snapshots written before it
	// existed, which decode as empty.
	DynamicActivations []DynamicHardforkActivation
}

// DynamicHardforkActivation is one (hardfork id, activation block) pair.
type DynamicHardforkActivation struct {
	ID    uint32
	Block uint32
}

// DynamicActivationBlock returns the recorded activation block of id.
func (h *HardforkPropertyObject) DynamicActivationBlock(id uint32) (uint32, bool) {
	for _, a := range h.DynamicActivations {
		if a.ID == id {
			return a.Block, true
		}
	}
	return 0, false
}

// SetDynamicActivation records (id, block), replacing an existing entry.
func (h *HardforkPropertyObject) SetDynamicActivation(id, block uint32) {
	for i := range h.DynamicActivations {
		if h.DynamicActivations[i].ID == id {
			h.DynamicActivations[i].Block = block
			return
		}
	}
	h.DynamicActivations = append(h.DynamicActivations, DynamicHardforkActivation{ID: id, Block: block})
	for i := len(h.DynamicActivations) - 1; i > 0 && h.DynamicActivations[i-1].ID > h.DynamicActivations[i].ID; i-- {
		h.DynamicActivations[i-1], h.DynamicActivations[i] = h.DynamicActivations[i], h.DynamicActivations[i-1]
	}
}

// AppendHardforkPropertyObject serializes h.
func AppendHardforkPropertyObject(dst []byte, h *HardforkPropertyObject) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(h.ProcessedHardforks)))
	for _, t := range h.ProcessedHardforks {
		dst = appendU32LE(dst, t)
	}
	dst = appendU32LE(dst, h.LastHardfork)
	dst = appendU32LE(dst, h.CurrentHardforkVersion)
	dst = appendU32LE(dst, h.NextHardfork)
	dst = appendU32LE(dst, h.NextHardforkTime)
	// Trailing, optional: only written when non-empty so a record with no
	// dynamic activations is byte-identical to the pre-field encoding.
	if len(h.DynamicActivations) > 0 {
		dst = fcraw.AppendVarUint(dst, uint32(len(h.DynamicActivations)))
		for _, a := range h.DynamicActivations {
			dst = appendU32LE(dst, a.ID)
			dst = appendU32LE(dst, a.Block)
		}
	}
	return dst
}

// ReadHardforkPropertyObject decodes h.
func ReadHardforkPropertyObject(src []byte) (HardforkPropertyObject, error) {
	r := fcraw.NewReader(src)
	var h HardforkPropertyObject
	count, err := r.ReadVarUint()
	if err != nil {
		return h, fmt.Errorf("hpo.processed_hardforks.len: %w", err)
	}
	if count > 0 {
		h.ProcessedHardforks = make([]uint32, 0, count)
		for i := uint32(0); i < count; i++ {
			t, err := r.ReadUint32()
			if err != nil {
				return h, fmt.Errorf("hpo.processed_hardforks[%d]: %w", i, err)
			}
			h.ProcessedHardforks = append(h.ProcessedHardforks, t)
		}
	}
	if h.LastHardfork, err = r.ReadUint32(); err != nil {
		return h, fmt.Errorf("hpo.last_hardfork: %w", err)
	}
	if h.CurrentHardforkVersion, err = r.ReadUint32(); err != nil {
		return h, fmt.Errorf("hpo.current_hardfork_version: %w", err)
	}
	if h.NextHardfork, err = r.ReadUint32(); err != nil {
		return h, fmt.Errorf("hpo.next_hardfork: %w", err)
	}
	if h.NextHardforkTime, err = r.ReadUint32(); err != nil {
		return h, fmt.Errorf("hpo.next_hardfork_time: %w", err)
	}
	if !r.AtEnd() {
		n, err := r.ReadVarUint()
		if err != nil {
			return h, fmt.Errorf("hpo.dynamic_activations.len: %w", err)
		}
		for i := uint32(0); i < n; i++ {
			var a DynamicHardforkActivation
			if a.ID, err = r.ReadUint32(); err != nil {
				return h, fmt.Errorf("hpo.dynamic_activations[%d].id: %w", i, err)
			}
			if a.Block, err = r.ReadUint32(); err != nil {
				return h, fmt.Errorf("hpo.dynamic_activations[%d].block: %w", i, err)
			}
			h.DynamicActivations = append(h.DynamicActivations, a)
		}
	}
	if !r.AtEnd() {
		return h, errors.New("hardfork_property: trailing bytes")
	}
	return h, nil
}

// PutHardforkPropertyObject stores h in the singleton slot.
func PutHardforkPropertyObject(s *state.Session, h *HardforkPropertyObject) {
	s.SerScratch = AppendHardforkPropertyObject(s.SerScratch[:0], h)
	s.Put(HardforkPropertyTable, state.PrimaryIndex, hardforkPropertySingletonKey, s.SerScratch)
}

// GetHardforkPropertyObject fetches the singleton.
func GetHardforkPropertyObject(s *state.Session) (HardforkPropertyObject, error) {
	v, err := s.Get(HardforkPropertyTable, state.PrimaryIndex, hardforkPropertySingletonKey)
	if err != nil {
		return HardforkPropertyObject{}, err
	}
	return ReadHardforkPropertyObject(v)
}

// HasHardforkPropertyObject reports existence.
func HasHardforkPropertyObject(s *state.Session) bool {
	has, _ := s.Has(HardforkPropertyTable, state.PrimaryIndex, hardforkPropertySingletonKey)
	return has
}
