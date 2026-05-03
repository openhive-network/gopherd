package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/state"
)

// RCPoolTable is the singleton holding the chain-wide rc_pool_object.
// Mirrors hive::chain::rc_pool_object — five per-resource manabars whose
// usage rate scales the per-tx RC cost.
//
// Activated at HF20. The pool size for each resource is recomputed every
// block from witness-voted parameters in resource_dynamics_params + the
// median chain_properties. This Go port stores enough to drive
// finalize_block accounting; finer pool dynamics land alongside the
// HF20 replay window.
const RCPoolTable state.TableID = 28

// rcPoolSingletonKey is the only valid key in the singleton table.
var rcPoolSingletonKey = []byte{0}

// HIVE_RC_NUM_RESOURCE_TYPES = 5 (libraries/protocol/include/hive/protocol/
// resource_credits.hpp).
const RCNumResources = 5

// RCResource enumerates the five resource types indexed by the pool slots.
type RCResource uint8

const (
	RCHistoryBytes  RCResource = 0
	RCNewAccounts   RCResource = 1
	RCMarketBytes   RCResource = 2
	RCStateBytes    RCResource = 3
	RCExecutionTime RCResource = 4
)

// RCPool mirrors rc_pool_object's per-resource manabar plus the cost
// coefficient used by the per-tx fee computation.
//
// Pool[i].Current = remaining capacity for that resource.
// Pool[i].Max     = maximum capacity (set by the rd_dynamic_params).
// Pool[i].Update  = last-update time_point_sec for regeneration.
// Pool[i].Cost    = per-resource cost coefficient (rc_per_resource).
type RCPool struct {
	Pool [RCNumResources]RCPoolEntry
}

type RCPoolEntry struct {
	Current int64
	Max     int64
	Update  uint32
	Cost    int64
}

// AppendRCPool serializes the pool. Order is fixed by the resource
// enumeration above so future readers can decode it positionally.
func AppendRCPool(dst []byte, p *RCPool) []byte {
	for i := 0; i < RCNumResources; i++ {
		dst = appendI64LE(dst, p.Pool[i].Current)
		dst = appendI64LE(dst, p.Pool[i].Max)
		dst = appendU32LE(dst, p.Pool[i].Update)
		dst = appendI64LE(dst, p.Pool[i].Cost)
	}
	return dst
}

// ReadRCPool decodes a pool from src.
func ReadRCPool(src []byte) (RCPool, error) {
	if len(src) < RCNumResources*(8+8+4+8) {
		return RCPool{}, fmt.Errorf("rc_pool: short read (%d bytes)", len(src))
	}
	r := fcraw.NewReader(src)
	var p RCPool
	for i := 0; i < RCNumResources; i++ {
		c, err := r.ReadInt64()
		if err != nil {
			return p, fmt.Errorf("rc_pool[%d].current: %w", i, err)
		}
		m, err := r.ReadInt64()
		if err != nil {
			return p, fmt.Errorf("rc_pool[%d].max: %w", i, err)
		}
		u, err := r.ReadUint32()
		if err != nil {
			return p, fmt.Errorf("rc_pool[%d].update: %w", i, err)
		}
		k, err := r.ReadInt64()
		if err != nil {
			return p, fmt.Errorf("rc_pool[%d].cost: %w", i, err)
		}
		p.Pool[i] = RCPoolEntry{Current: c, Max: m, Update: u, Cost: k}
	}
	if !r.AtEnd() {
		return p, errors.New("rc_pool: trailing bytes")
	}
	return p, nil
}

// PutRCPool stores the singleton.
func PutRCPool(s *state.Session, p *RCPool) {
	s.Put(RCPoolTable, state.PrimaryIndex, rcPoolSingletonKey, AppendRCPool(nil, p))
}

// GetRCPool fetches the singleton; ErrNotFound until HF20 initialization.
func GetRCPool(s *state.Session) (RCPool, error) {
	v, err := s.Get(RCPoolTable, state.PrimaryIndex, rcPoolSingletonKey)
	if err != nil {
		return RCPool{}, err
	}
	return ReadRCPool(v)
}

// HasRCPool reports existence (used by the HF20 init guard).
func HasRCPool(s *state.Session) bool {
	has, _ := s.Has(RCPoolTable, state.PrimaryIndex, rcPoolSingletonKey)
	return has
}
