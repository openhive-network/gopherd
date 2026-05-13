package objects

import (
	"hived-go/internal/state"
)

// RCPoolTable is the singleton holding the chain-wide rc_pool_object.
// Mirrors hive::chain::rc_pool_object.
//
// Activated at HF20. The struct + codec live in rc_objects.go (Phase 1
// foundation); this file holds the table id, resource enumeration and
// singleton key.
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

// RCPoolEntry is the legacy per-resource manabar shape (kept for the
// scaffolding pre-HF20 init code in rc_finalize.go). The C++-faithful
// rc_pool_object payload (PoolArray / UsageInWindow / LastKnownBudget /
// ResourceWeights / SumOfResourceWeights / LastRecalcTime) sits on the
// RCPool struct in rc_objects.go; both shapes co-exist on the wire so
// older serialized state still decodes.
type RCPoolEntry struct {
	Current int64
	Max     int64
	Update  uint32
	Cost    int64
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
