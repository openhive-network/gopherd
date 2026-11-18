package objects

import (
	"errors"
	"fmt"

	"gopherd/internal/fcraw"
	"gopherd/internal/state"
)

// StateStampDataTable is the singleton table for state_stamp_data_object
// (hived e2100feda / e78bfabd1,
// libraries/chain/include/hive/chain/detail/state/state_stamp_data_object.hpp).
const StateStampDataTable state.TableID = 41

var stateStampDataKey = []byte{0}

// StateStampData is hived's state_stamp_data_object: the digests ("stamps")
// of consensus state published since the last stamp-carrying block, in
// publication order.
//
// hived creates the object at genesis and publishes into it from then on
// (today only resource_credits::handle_auto_report, once per RC stats window
// since HF20). Blocks may carry the merkle root of the list only from HF29,
// and nothing resets it before then, so the first block after HF29 commits
// every stamp published since HF20 — about one per day.
//
// Incomplete is gopherd's own: hived has no counterpart. It marks a list
// that was created after stamps had already been published elsewhere in the
// chain's history, i.e. in a state written by a gopherd that did not keep
// the list yet. Such a list can never produce the root hived expects for the
// first HF29 block, so the HF29 check refuses to proceed on it (the state
// has to be rebuilt by a replay from before HF20) instead of failing later
// with a bare root mismatch, or worse, producing a block with a wrong root.
type StateStampData struct {
	Stamps     [][32]byte
	Incomplete bool
}

// AppendStateStampData serializes d: a flags byte (bit 0 = Incomplete), then
// the stamps as a varint count of 32-byte digests.
func AppendStateStampData(dst []byte, d *StateStampData) []byte {
	var flags byte
	if d.Incomplete {
		flags |= 1
	}
	dst = append(dst, flags)
	dst = fcraw.AppendVarUint(dst, uint32(len(d.Stamps)))
	for i := range d.Stamps {
		dst = append(dst, d.Stamps[i][:]...)
	}
	return dst
}

// ReadStateStampData decodes a row written by AppendStateStampData.
func ReadStateStampData(src []byte) (StateStampData, error) {
	var d StateStampData
	r := fcraw.NewReader(src)
	flags, err := r.ReadByte()
	if err != nil {
		return d, fmt.Errorf("state_stamp_data.flags: %w", err)
	}
	if flags&^1 != 0 {
		return d, fmt.Errorf("state_stamp_data.flags: unknown bits %#x", flags)
	}
	d.Incomplete = flags&1 != 0
	n, err := r.ReadVarUint()
	if err != nil {
		return d, fmt.Errorf("state_stamp_data.stamps.len: %w", err)
	}
	if err := r.CheckCount(n, 32); err != nil {
		return d, fmt.Errorf("state_stamp_data.stamps: %w", err)
	}
	if n > 0 {
		d.Stamps = make([][32]byte, n)
	}
	for i := range d.Stamps {
		if err := r.ReadInto(d.Stamps[i][:]); err != nil {
			return d, fmt.Errorf("state_stamp_data.stamps[%d]: %w", i, err)
		}
	}
	if r.Remaining() != 0 {
		return d, fmt.Errorf("state_stamp_data: %d trailing bytes", r.Remaining())
	}
	return d, nil
}

// PutStateStampData stores the singleton.
func PutStateStampData(s *state.Session, d *StateStampData) {
	s.Put(StateStampDataTable, state.PrimaryIndex, stateStampDataKey, AppendStateStampData(nil, d))
}

// GetStateStampData loads the singleton. ok is false when the row does not
// exist, which on mainnet means a state written before gopherd kept it.
func GetStateStampData(s *state.Session) (d StateStampData, ok bool, err error) {
	v, err := s.Get(StateStampDataTable, state.PrimaryIndex, stateStampDataKey)
	if errors.Is(err, state.ErrNotFound) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	d, err = ReadStateStampData(v)
	return d, true, err
}
