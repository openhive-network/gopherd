package chain

import (
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// adjustSupplyHive mirrors C++ database::adjust_supply(HIVE_asset)
// (libraries/chain/database.cpp:3480). Applies a HIVE delta to
// current_supply AND virtual_supply incrementally — no full recompute.
// `adjust_vesting` is unused in our era (only the genesis-week bootstrap
// branch in C++ uses it; current_supply is the only thing it modifies).
func adjustSupplyHive(s *state.Session, dgpo *objects.DynamicGlobalProperty, delta int64) {
	dgpo.CurrentSupply += delta
	dgpo.VirtualSupply += delta
}

// adjustSupplyHbd mirrors C++ database::adjust_supply(HBD_asset)
// (libraries/chain/database.cpp:3496). Applies a HBD delta to
// current_hbd_supply, then FULL RECOMPUTE of virtual_supply from
// current_supply + current_hbd_supply × median_history. The recompute
// is the key behavioural difference from a simple incremental update —
// it eliminates intra-block VS drift between the HIVE-side and HBD-side
// adjustments that share a payCreateHbd / processConversions /
// AdjustHBDBalance / clearNullAccountBalance call. Without it, Go and
// C++ diverge by `hbd × median` per HBD adjustment until the next
// updateVirtualSupply at end-of-block.
//
// The median feed is read each call; in pre-feed era it returns 0 and
// virtual_supply collapses to current_supply (matching C++'s
// median_price.is_null() branch in update_virtual_supply).
func adjustSupplyHbd(s *state.Session, dgpo *objects.DynamicGlobalProperty, delta int64) {
	dgpo.CurrentHBDSupply += delta
	dgpo.VirtualSupply = dgpo.CurrentSupply
	if fh, ferr := objects.GetFeedHistory(s); ferr == nil && !priceIsZero(fh.CurrentMedianHistory) {
		dgpo.VirtualSupply += convertHBDToHive(dgpo.CurrentHBDSupply, fh.CurrentMedianHistory)
	}
}
