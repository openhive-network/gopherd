package objects

import (
	"encoding/binary"
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/state"
)

// DynamicGlobalPropertyTable is the TableID for the singleton
// dynamic_global_property_object.
const DynamicGlobalPropertyTable state.TableID = 0

// dgpoSingletonKey is the only valid primary key inside the DGPO table.
// Stored as a fixed all-zero 1-byte value so the row is trivially seekable.
var dgpoSingletonKey = []byte{0}

// DynamicGlobalProperty mirrors the hived `dynamic_global_property_object`
// fields most evaluators read or update. Field set is incremental — add
// fields as evaluators require them.
//
// The on-disk schema for this struct is implementation-internal (Hive
// has no state-root, so we are free to add fields).
type DynamicGlobalProperty struct {
	HeadBlockNumber uint32
	HeadBlockID     [20]byte
	Time            uint32 // time_point_sec
	CurrentWitness  string

	// Total liquid supply, in satoshis (1e-3 HIVE).
	CurrentSupply int64
	// Total HBD supply, in satoshis.
	CurrentHBDSupply int64
	// Total VESTS in circulation.
	TotalVestingShares int64
}

// AppendDGPO serializes p to dst.
func AppendDGPO(dst []byte, p *DynamicGlobalProperty) []byte {
	dst = appendU32LE(dst, p.HeadBlockNumber)
	dst = append(dst, p.HeadBlockID[:]...)
	dst = appendU32LE(dst, p.Time)
	dst = fcraw.AppendVarUint(dst, uint32(len(p.CurrentWitness)))
	dst = append(dst, p.CurrentWitness...)
	dst = appendI64LE(dst, p.CurrentSupply)
	dst = appendI64LE(dst, p.CurrentHBDSupply)
	dst = appendI64LE(dst, p.TotalVestingShares)
	return dst
}

// ReadDGPO decodes a DGPO from src.
func ReadDGPO(src []byte) (DynamicGlobalProperty, error) {
	r := fcraw.NewReader(src)
	var p DynamicGlobalProperty
	var err error
	if p.HeadBlockNumber, err = r.ReadUint32(); err != nil {
		return p, fmt.Errorf("dgpo.head_block_number: %w", err)
	}
	if err := r.ReadInto(p.HeadBlockID[:]); err != nil {
		return p, fmt.Errorf("dgpo.head_block_id: %w", err)
	}
	if p.Time, err = r.ReadUint32(); err != nil {
		return p, fmt.Errorf("dgpo.time: %w", err)
	}
	if p.CurrentWitness, err = r.ReadString(); err != nil {
		return p, fmt.Errorf("dgpo.current_witness: %w", err)
	}
	if p.CurrentSupply, err = r.ReadInt64(); err != nil {
		return p, fmt.Errorf("dgpo.current_supply: %w", err)
	}
	if p.CurrentHBDSupply, err = r.ReadInt64(); err != nil {
		return p, fmt.Errorf("dgpo.current_hbd_supply: %w", err)
	}
	if p.TotalVestingShares, err = r.ReadInt64(); err != nil {
		return p, fmt.Errorf("dgpo.total_vesting_shares: %w", err)
	}
	if !r.AtEnd() {
		return p, errors.New("dgpo: trailing bytes after decode (schema mismatch?)")
	}
	return p, nil
}

// PutDGPO stores p in the singleton slot.
func PutDGPO(s *state.Session, p *DynamicGlobalProperty) {
	s.Put(DynamicGlobalPropertyTable, state.PrimaryIndex, dgpoSingletonKey, AppendDGPO(nil, p))
}

// GetDGPO fetches the singleton dynamic_global_property_object.
func GetDGPO(s *state.Session) (DynamicGlobalProperty, error) {
	v, err := s.Get(DynamicGlobalPropertyTable, state.PrimaryIndex, dgpoSingletonKey)
	if err != nil {
		return DynamicGlobalProperty{}, err
	}
	return ReadDGPO(v)
}

// --- private helpers ----------------------------------------------------

func appendU32LE(dst []byte, v uint32) []byte {
	return binary.LittleEndian.AppendUint32(dst, v)
}

func appendI64LE(dst []byte, v int64) []byte {
	return binary.LittleEndian.AppendUint64(dst, uint64(v))
}
