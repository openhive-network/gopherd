// Package objects — RC subsystem Phase 1 state objects + FC codecs.
//
// Mirrors hive::chain::rc_*_object in
// libraries/chain/include/hive/chain/rc/rc_objects.hpp.
//
// Six state objects are defined here:
//
//   1. RCResourceParam       — singleton  (rc_resource_param_object)
//   2. RCPool                — singleton  (rc_pool_object)
//   3. RCStats               — dual: pending + archive (rc_stats_object)
//   4. RCDirectDelegation    — by (from, to)  (rc_direct_delegation_object)
//   5. RCExpiredDelegation   — by from        (rc_expired_delegation_object)
//   6. RCUsageBucket         — 24 hourly buckets (rc_usage_bucket_object)
//
// The RC subsystem activates at HF20 (~block 26,200,000) and remains
// dormant for the current replay window. The codecs here are the
// scaffolding required by upcoming phases — finalize_block hooks
// (Phase 2), delegate_rc evaluator completion (Phase 3), set_pool_params
// + set_slot_delegator (Phase 4) and full replay integration (Phase 5).
//
// C++ → Go translation notes for exotic FC types:
//   * fc::int_array<T, N>      → fixed-size Go array [N]T.
//   * fc::restartable_sha256   → 32-byte [32]byte digest field (we
//                                serialize the raw digest; the rolling
//                                state is rebuilt at replay).
//   * resource_count_type      → [RCNumResources]int64 (= [5]int64).
//   * resource_share_type      → [RCNumResources]uint16.
//   * rd_dynamics_params       → flattened struct ResourceDynamics
//                                with the 6 scalar fields C++ tracks.
//   * rc_price_curve_params    → flattened struct PriceCurve.
//
// All multi-byte ints are little-endian on the wire (matches FC).
package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// -----------------------------------------------------------------------------
// Table IDs (sequential after the existing tables; see other *_objects.go).
// -----------------------------------------------------------------------------

const (
	// RCResourceParamTable holds the singleton rc_resource_param_object.
	RCResourceParamTable state.TableID = 30
	// RCStatsTable holds rc_stats_object — exactly two rows:
	// RCStatsIDPending (0) and RCStatsIDArchive (1).
	RCStatsTable state.TableID = 31
	// RCExpiredDelegationTable holds rc_expired_delegation_object rows
	// (one per delegator with overflow).
	RCExpiredDelegationTable state.TableID = 32
	// RCUsageBucketTable holds the 24 hourly rc_usage_bucket_object rows.
	RCUsageBucketTable state.TableID = 33
)

// -----------------------------------------------------------------------------
// Secondary indexes.
// -----------------------------------------------------------------------------

const (
	// RCDirectDelegationByToIndex orders direct delegations by (to, from),
	// supporting reverse lookups during recipient-side regen / removal.
	RCDirectDelegationByToIndex state.IndexID = 1

	// RCExpiredDelegationByFromIndex maps from → expired-delegation row
	// (one-to-one — `by_account` in the C++ MIC).
	RCExpiredDelegationByFromIndex state.IndexID = 1

	// RCUsageBucketBySeqnoIndex orders usage buckets by their rolling
	// sequence number (used to find the current/oldest bucket during the
	// per-block rotation). Mirrors `by_timestamp` in the C++ MIC.
	RCUsageBucketBySeqnoIndex state.IndexID = 1
)

// -----------------------------------------------------------------------------
// Resource enumeration & constants.
// -----------------------------------------------------------------------------
//
// NOTE: RCResource, RCNumResources, RCHistoryBytes, RCNewAccounts,
// RCMarketBytes, RCStateBytes, RCExecutionTime are defined in rc_pool.go
// (kept there to preserve the pre-existing public API). They're referenced
// throughout this file.

// RCNumOperations mirrors HIVE_RC_NUM_OPERATIONS — the tag value of
// fill_convert_request_operation = 50 (first virtual op). The OpStats
// array has size RCNumOperations+1; the trailing slot bins multi-op
// transactions.
const RCNumOperations = 50

// RCNumPayerRanks mirrors HIVE_RC_NUM_PAYER_RANKS — 8 logarithmic
// buckets of RC mana levels reported by daily stats.
const RCNumPayerRanks = 8

// RCWindowBucketCount mirrors HIVE_RC_WINDOW_BUCKET_COUNT — 24 hourly
// buckets form the trailing usage window.
const RCWindowBucketCount = 24

// RCStatsIDPending / RCStatsIDArchive identify the two rc_stats_object
// instances (RC_PENDING_STATS_ID / RC_ARCHIVE_STATS_ID in C++).
const (
	RCStatsIDPending uint8 = 0
	RCStatsIDArchive uint8 = 1
)

// rcSingletonKey is the singleton-key sentinel for RC singleton tables
// (RCResourceParam). Matches dgpoSingletonKey-style convention.
var rcSingletonKey = []byte{0}

// -----------------------------------------------------------------------------
// 1. RCResourceParam (singleton).
// -----------------------------------------------------------------------------

// ResourceDynamics mirrors util::rd_dynamics_params — the per-resource
// pool dynamics (pool_eq target, max size, decay, regen budget).
type ResourceDynamics struct {
	// ResourceUnit scales raw resource counts (bytes, accounts, etc.)
	// into pool-space.
	ResourceUnit uint64
	// BudgetPerTime is the per-block regeneration rate.
	BudgetPerTime int32
	// PoolEq is the equilibrium pool level (target).
	PoolEq uint64
	// MaxPool clamps the pool's maximum.
	MaxPool uint64
	// DecayPerTime + DecayDenomShift describe pool decay toward PoolEq.
	// Together they replace rd_decay_params (flattened here so the
	// state object has a flat scalar layout).
	DecayPerTime    uint32
	DecayDenomShift uint8
	// MinDecay is the floor on per-block decay (zero in current config).
	MinDecay int64
}

// PriceCurve mirrors rc_price_curve_params — the cost function applied
// when pool depletion lifts the per-unit price.
type PriceCurve struct {
	CoeffA uint64
	CoeffB uint64
	Shift  uint8
}

// RCResourceEntry pairs the dynamics + price curve for one resource.
// Mirrors hive::chain::rc_resource_params.
type RCResourceEntry struct {
	Dynamics ResourceDynamics
	Curve    PriceCurve
}

// RCResourceParam is the singleton rc_resource_param_object — five
// per-resource (dynamics, curve) entries indexed by RCResource.
type RCResourceParam struct {
	Entries [RCNumResources]RCResourceEntry
}

func appendResourceDynamics(dst []byte, d *ResourceDynamics) []byte {
	dst = appendU64LE(dst, d.ResourceUnit)
	dst = appendI32LE(dst, d.BudgetPerTime)
	dst = appendU64LE(dst, d.PoolEq)
	dst = appendU64LE(dst, d.MaxPool)
	dst = appendU32LE(dst, d.DecayPerTime)
	dst = append(dst, d.DecayDenomShift)
	return appendI64LE(dst, d.MinDecay)
}

func readResourceDynamics(r *fcraw.Reader) (ResourceDynamics, error) {
	var d ResourceDynamics
	var err error
	if d.ResourceUnit, err = r.ReadUint64(); err != nil {
		return d, err
	}
	if d.BudgetPerTime, err = r.ReadInt32(); err != nil {
		return d, err
	}
	if d.PoolEq, err = r.ReadUint64(); err != nil {
		return d, err
	}
	if d.MaxPool, err = r.ReadUint64(); err != nil {
		return d, err
	}
	if d.DecayPerTime, err = r.ReadUint32(); err != nil {
		return d, err
	}
	if d.DecayDenomShift, err = r.ReadUint8(); err != nil {
		return d, err
	}
	if d.MinDecay, err = r.ReadInt64(); err != nil {
		return d, err
	}
	return d, nil
}

func appendPriceCurve(dst []byte, c *PriceCurve) []byte {
	dst = appendU64LE(dst, c.CoeffA)
	dst = appendU64LE(dst, c.CoeffB)
	return append(dst, c.Shift)
}

func readPriceCurve(r *fcraw.Reader) (PriceCurve, error) {
	var c PriceCurve
	var err error
	if c.CoeffA, err = r.ReadUint64(); err != nil {
		return c, err
	}
	if c.CoeffB, err = r.ReadUint64(); err != nil {
		return c, err
	}
	if c.Shift, err = r.ReadUint8(); err != nil {
		return c, err
	}
	return c, nil
}

// AppendRCResourceParam serializes p.
func AppendRCResourceParam(dst []byte, p *RCResourceParam) []byte {
	for i := 0; i < RCNumResources; i++ {
		dst = appendResourceDynamics(dst, &p.Entries[i].Dynamics)
		dst = appendPriceCurve(dst, &p.Entries[i].Curve)
	}
	return dst
}

// ReadRCResourceParam decodes p.
func ReadRCResourceParam(src []byte) (RCResourceParam, error) {
	r := fcraw.NewReader(src)
	var p RCResourceParam
	for i := 0; i < RCNumResources; i++ {
		d, err := readResourceDynamics(r)
		if err != nil {
			return p, fmt.Errorf("rc_resource_param[%d].dynamics: %w", i, err)
		}
		c, err := readPriceCurve(r)
		if err != nil {
			return p, fmt.Errorf("rc_resource_param[%d].curve: %w", i, err)
		}
		p.Entries[i] = RCResourceEntry{Dynamics: d, Curve: c}
	}
	if !r.AtEnd() {
		return p, errors.New("rc_resource_param: trailing bytes")
	}
	return p, nil
}

// PutRCResourceParam stores the singleton.
func PutRCResourceParam(s *state.Session, p *RCResourceParam) {
	s.Put(RCResourceParamTable, state.PrimaryIndex, rcSingletonKey,
		AppendRCResourceParam(nil, p))
}

// GetRCResourceParam fetches the singleton.
func GetRCResourceParam(s *state.Session) (RCResourceParam, error) {
	v, err := s.Get(RCResourceParamTable, state.PrimaryIndex, rcSingletonKey)
	if err != nil {
		return RCResourceParam{}, err
	}
	return ReadRCResourceParam(v)
}

// HasRCResourceParam reports existence (HF20-gated init).
func HasRCResourceParam(s *state.Session) bool {
	has, _ := s.Has(RCResourceParamTable, state.PrimaryIndex, rcSingletonKey)
	return has
}

// -----------------------------------------------------------------------------
// 2. RCPool (singleton).
// -----------------------------------------------------------------------------
//
// Replaces the earlier stub layout. The C++ rc_pool_object stores five
// flat arrays plus a recalc timestamp; we mirror that exactly so finalize_block
// hooks can update them positionally without recomputing aliases.

// RCPool mirrors rc_pool_object — the chain-wide singleton holding
// per-resource pool sizes, in-window usage, last-known budgets and
// derived resource weights (basis-point share of global RC inflation).
type RCPool struct {
	// PoolArray[i] is the current size of pool i (history_bytes…
	// execution_time). Drops as transactions consume the resource,
	// grows back via budget_per_time replenishment.
	PoolArray [RCNumResources]int64
	// UsageInWindow[i] is the rolling 24-bucket sum of consumption.
	UsageInWindow [RCNumResources]int64
	// LastKnownBudget[i] records the per-block budget from the last
	// recompute; used to detect when params changed.
	LastKnownBudget [RCNumResources]int64
	// ResourceWeights[i] is the basis-point share (out of 10000) that
	// resource i takes from the global RC inflation. The new-accounts
	// slot is set to SumOfResourceWeights (always 100%).
	ResourceWeights [RCNumResources]uint16
	// SumOfResourceWeights is the divisor when computing per-resource
	// share. Always ≥ 1 (zero is invalid — see C++ init).
	SumOfResourceWeights uint64
	// LastRecalcTime is the time_point_sec of the most recent
	// recalculate_resource_weights() call (the per-block hook target).
	// Tracked here so dormant pre-HF20 sessions can still write a
	// stable value.
	LastRecalcTime uint32

	// -------------------------------------------------------------------
	// LEGACY SHIM — preserves the earlier RCPoolEntry shape so the
	// active rc_finalize.go code (initRCPoolIfNeeded) continues to
	// compile while the Phase 2 hooks land. Drop once those callers
	// migrate.
	// -------------------------------------------------------------------
	Pool [RCNumResources]RCPoolEntry
}

// AppendRCPool serializes p.
//
// Wire order is the C++ FC_REFLECT order (id is implicit / external):
//
//	pool_array, usage_in_window, last_known_budget,
//	resource_weights, sum_of_resource_weights, last_recalc_time, legacy pool.
//
// The trailing legacy block keeps round-trip compat with the prior
// rc_pool.go layout until Phase 2 ports the C++ pool mutation paths.
func AppendRCPool(dst []byte, p *RCPool) []byte {
	for i := 0; i < RCNumResources; i++ {
		dst = appendI64LE(dst, p.PoolArray[i])
	}
	for i := 0; i < RCNumResources; i++ {
		dst = appendI64LE(dst, p.UsageInWindow[i])
	}
	for i := 0; i < RCNumResources; i++ {
		dst = appendI64LE(dst, p.LastKnownBudget[i])
	}
	for i := 0; i < RCNumResources; i++ {
		dst = appendU16LE(dst, p.ResourceWeights[i])
	}
	dst = appendU64LE(dst, p.SumOfResourceWeights)
	dst = appendU32LE(dst, p.LastRecalcTime)
	for i := 0; i < RCNumResources; i++ {
		dst = appendI64LE(dst, p.Pool[i].Current)
		dst = appendI64LE(dst, p.Pool[i].Max)
		dst = appendU32LE(dst, p.Pool[i].Update)
		dst = appendI64LE(dst, p.Pool[i].Cost)
	}
	return dst
}

// ReadRCPool decodes a pool. Older payloads (predating the canonical
// layout) consist of just the legacy block — when the input is exactly
// that size we fall back to legacy-only decode.
func ReadRCPool(src []byte) (RCPool, error) {
	const legacyOnlySize = RCNumResources * (8 + 8 + 4 + 8) // 140
	var p RCPool
	if len(src) == legacyOnlySize {
		r := fcraw.NewReader(src)
		for i := 0; i < RCNumResources; i++ {
			c, err := r.ReadInt64()
			if err != nil {
				return p, err
			}
			m, err := r.ReadInt64()
			if err != nil {
				return p, err
			}
			u, err := r.ReadUint32()
			if err != nil {
				return p, err
			}
			k, err := r.ReadInt64()
			if err != nil {
				return p, err
			}
			p.Pool[i] = RCPoolEntry{Current: c, Max: m, Update: u, Cost: k}
		}
		return p, nil
	}
	r := fcraw.NewReader(src)
	var err error
	for i := 0; i < RCNumResources; i++ {
		if p.PoolArray[i], err = r.ReadInt64(); err != nil {
			return p, fmt.Errorf("rc_pool.pool_array[%d]: %w", i, err)
		}
	}
	for i := 0; i < RCNumResources; i++ {
		if p.UsageInWindow[i], err = r.ReadInt64(); err != nil {
			return p, fmt.Errorf("rc_pool.usage_in_window[%d]: %w", i, err)
		}
	}
	for i := 0; i < RCNumResources; i++ {
		if p.LastKnownBudget[i], err = r.ReadInt64(); err != nil {
			return p, fmt.Errorf("rc_pool.last_known_budget[%d]: %w", i, err)
		}
	}
	for i := 0; i < RCNumResources; i++ {
		if p.ResourceWeights[i], err = r.ReadUint16(); err != nil {
			return p, fmt.Errorf("rc_pool.resource_weights[%d]: %w", i, err)
		}
	}
	if p.SumOfResourceWeights, err = r.ReadUint64(); err != nil {
		return p, fmt.Errorf("rc_pool.sum_of_resource_weights: %w", err)
	}
	if p.LastRecalcTime, err = r.ReadUint32(); err != nil {
		return p, fmt.Errorf("rc_pool.last_recalc_time: %w", err)
	}
	for i := 0; i < RCNumResources; i++ {
		c, err := r.ReadInt64()
		if err != nil {
			return p, fmt.Errorf("rc_pool.legacy[%d].current: %w", i, err)
		}
		m, err := r.ReadInt64()
		if err != nil {
			return p, fmt.Errorf("rc_pool.legacy[%d].max: %w", i, err)
		}
		u, err := r.ReadUint32()
		if err != nil {
			return p, fmt.Errorf("rc_pool.legacy[%d].update: %w", i, err)
		}
		k, err := r.ReadInt64()
		if err != nil {
			return p, fmt.Errorf("rc_pool.legacy[%d].cost: %w", i, err)
		}
		p.Pool[i] = RCPoolEntry{Current: c, Max: m, Update: u, Cost: k}
	}
	if !r.AtEnd() {
		return p, errors.New("rc_pool: trailing bytes")
	}
	return p, nil
}

// -----------------------------------------------------------------------------
// 3. RCStats (two instances: pending = 0, archive = 1).
// -----------------------------------------------------------------------------

// RCOpStats mirrors rc_op_stats — per-op-tag cumulative stats. The
// average_cost() method on the C++ side is computed on demand, not
// stored (matches that: callers derive avg from Cost / Count).
type RCOpStats struct {
	Usage [RCNumResources]int64
	Cost  [RCNumResources]int64
	Count uint32
}

// RCPayerStats mirrors rc_payer_stats — per-rank cumulative payer info.
type RCPayerStats struct {
	Usage             [RCNumResources]int64
	Cost              [RCNumResources]int64
	LessThan5Percent  uint32
	LessThan10Percent uint32
	LessThan20Percent uint32
	// CantAfford[3] = was-observed-with-less-than-average-cost for the
	// three "headline" ops (vote, comment, transfer).
	CantAfford [3]uint32
	Count      uint32
}

// RCStats mirrors rc_stats_object. Two instances exist; the per-block
// finalize_block hook archives the pending row into the archive slot.
type RCStats struct {
	BlockNum    uint32
	Regen       int64
	Stamp       [32]byte // raw sha256 digest of the rolling stamp
	Budget      [RCNumResources]int64
	Pool        [RCNumResources]int64
	Share       [RCNumResources]uint16
	OpStats     [RCNumOperations + 1]RCOpStats
	PayerStats  [RCNumPayerRanks]RCPayerStats
	AverageCost [3]int64
}

func appendOpStats(dst []byte, s *RCOpStats) []byte {
	for i := 0; i < RCNumResources; i++ {
		dst = appendI64LE(dst, s.Usage[i])
	}
	for i := 0; i < RCNumResources; i++ {
		dst = appendI64LE(dst, s.Cost[i])
	}
	return appendU32LE(dst, s.Count)
}

func readOpStats(r *fcraw.Reader) (RCOpStats, error) {
	var s RCOpStats
	var err error
	for i := 0; i < RCNumResources; i++ {
		if s.Usage[i], err = r.ReadInt64(); err != nil {
			return s, err
		}
	}
	for i := 0; i < RCNumResources; i++ {
		if s.Cost[i], err = r.ReadInt64(); err != nil {
			return s, err
		}
	}
	if s.Count, err = r.ReadUint32(); err != nil {
		return s, err
	}
	return s, nil
}

func appendPayerStats(dst []byte, s *RCPayerStats) []byte {
	for i := 0; i < RCNumResources; i++ {
		dst = appendI64LE(dst, s.Usage[i])
	}
	for i := 0; i < RCNumResources; i++ {
		dst = appendI64LE(dst, s.Cost[i])
	}
	dst = appendU32LE(dst, s.LessThan5Percent)
	dst = appendU32LE(dst, s.LessThan10Percent)
	dst = appendU32LE(dst, s.LessThan20Percent)
	for i := 0; i < 3; i++ {
		dst = appendU32LE(dst, s.CantAfford[i])
	}
	return appendU32LE(dst, s.Count)
}

func readPayerStats(r *fcraw.Reader) (RCPayerStats, error) {
	var s RCPayerStats
	var err error
	for i := 0; i < RCNumResources; i++ {
		if s.Usage[i], err = r.ReadInt64(); err != nil {
			return s, err
		}
	}
	for i := 0; i < RCNumResources; i++ {
		if s.Cost[i], err = r.ReadInt64(); err != nil {
			return s, err
		}
	}
	if s.LessThan5Percent, err = r.ReadUint32(); err != nil {
		return s, err
	}
	if s.LessThan10Percent, err = r.ReadUint32(); err != nil {
		return s, err
	}
	if s.LessThan20Percent, err = r.ReadUint32(); err != nil {
		return s, err
	}
	for i := 0; i < 3; i++ {
		if s.CantAfford[i], err = r.ReadUint32(); err != nil {
			return s, err
		}
	}
	if s.Count, err = r.ReadUint32(); err != nil {
		return s, err
	}
	return s, nil
}

// AppendRCStats serializes s.
func AppendRCStats(dst []byte, s *RCStats) []byte {
	dst = appendU32LE(dst, s.BlockNum)
	dst = appendI64LE(dst, s.Regen)
	dst = append(dst, s.Stamp[:]...)
	for i := 0; i < RCNumResources; i++ {
		dst = appendI64LE(dst, s.Budget[i])
	}
	for i := 0; i < RCNumResources; i++ {
		dst = appendI64LE(dst, s.Pool[i])
	}
	for i := 0; i < RCNumResources; i++ {
		dst = appendU16LE(dst, s.Share[i])
	}
	for i := 0; i < RCNumOperations+1; i++ {
		dst = appendOpStats(dst, &s.OpStats[i])
	}
	for i := 0; i < RCNumPayerRanks; i++ {
		dst = appendPayerStats(dst, &s.PayerStats[i])
	}
	for i := 0; i < 3; i++ {
		dst = appendI64LE(dst, s.AverageCost[i])
	}
	return dst
}

// ReadRCStats decodes s.
func ReadRCStats(src []byte) (RCStats, error) {
	r := fcraw.NewReader(src)
	var s RCStats
	var err error
	if s.BlockNum, err = r.ReadUint32(); err != nil {
		return s, fmt.Errorf("rc_stats.block_num: %w", err)
	}
	if s.Regen, err = r.ReadInt64(); err != nil {
		return s, fmt.Errorf("rc_stats.regen: %w", err)
	}
	stamp, err := r.ReadRaw(32)
	if err != nil {
		return s, fmt.Errorf("rc_stats.stamp: %w", err)
	}
	copy(s.Stamp[:], stamp)
	for i := 0; i < RCNumResources; i++ {
		if s.Budget[i], err = r.ReadInt64(); err != nil {
			return s, fmt.Errorf("rc_stats.budget[%d]: %w", i, err)
		}
	}
	for i := 0; i < RCNumResources; i++ {
		if s.Pool[i], err = r.ReadInt64(); err != nil {
			return s, fmt.Errorf("rc_stats.pool[%d]: %w", i, err)
		}
	}
	for i := 0; i < RCNumResources; i++ {
		if s.Share[i], err = r.ReadUint16(); err != nil {
			return s, fmt.Errorf("rc_stats.share[%d]: %w", i, err)
		}
	}
	for i := 0; i < RCNumOperations+1; i++ {
		if s.OpStats[i], err = readOpStats(r); err != nil {
			return s, fmt.Errorf("rc_stats.op_stats[%d]: %w", i, err)
		}
	}
	for i := 0; i < RCNumPayerRanks; i++ {
		if s.PayerStats[i], err = readPayerStats(r); err != nil {
			return s, fmt.Errorf("rc_stats.payer_stats[%d]: %w", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		if s.AverageCost[i], err = r.ReadInt64(); err != nil {
			return s, fmt.Errorf("rc_stats.average_cost[%d]: %w", i, err)
		}
	}
	if !r.AtEnd() {
		return s, errors.New("rc_stats: trailing bytes")
	}
	return s, nil
}

func rcStatsKey(id uint8) []byte { return []byte{id} }

// PutRCStats stores the row keyed by id (RCStatsIDPending or
// RCStatsIDArchive).
func PutRCStats(s *state.Session, id uint8, row *RCStats) {
	s.Put(RCStatsTable, state.PrimaryIndex, rcStatsKey(id), AppendRCStats(nil, row))
}

// GetRCStats fetches a row by id.
func GetRCStats(s *state.Session, id uint8) (RCStats, error) {
	v, err := s.Get(RCStatsTable, state.PrimaryIndex, rcStatsKey(id))
	if err != nil {
		return RCStats{}, err
	}
	return ReadRCStats(v)
}

// HasRCStats reports existence of a stats row.
func HasRCStats(s *state.Session, id uint8) bool {
	has, _ := s.Has(RCStatsTable, state.PrimaryIndex, rcStatsKey(id))
	return has
}

// -----------------------------------------------------------------------------
// 4. RCDirectDelegation — see rc_delegation.go for the storage type and
//    primary-key codec. The C++ schema also exposes a by_from_to composite;
//    in Go that's the primary key. Below we add the symmetric by_to index.
// -----------------------------------------------------------------------------

// rcDirectDelegationByToKey orders rows by (to, from), enabling the
// recipient-side enumeration needed by Phase 3 cleanup / recipient mana
// recompute.
func rcDirectDelegationByToKey(to, from auth.AccountName) []byte {
	out := state.AppendAccountName(nil, to)
	return state.AppendAccountName(out, from)
}

// PutRCDirectDelegationFull stores d and also maintains the by_to index.
// Phase 1 keeps the simpler PutRCDirectDelegation (primary-only) intact
// so existing callers don't immediately need to migrate; callers that
// need recipient-side enumeration use this variant.
func PutRCDirectDelegationFull(s *state.Session, d *RCDirectDelegation) {
	PutRCDirectDelegation(s, d)
	s.Put(RCDirectDelegationTable, RCDirectDelegationByToIndex,
		rcDirectDelegationByToKey(d.To, d.From), nil)
}

// DeleteRCDirectDelegationFull removes both primary and by_to entries.
func DeleteRCDirectDelegationFull(s *state.Session, from, to auth.AccountName) {
	DeleteRCDirectDelegation(s, from, to)
	s.Delete(RCDirectDelegationTable, RCDirectDelegationByToIndex,
		rcDirectDelegationByToKey(to, from))
}

// -----------------------------------------------------------------------------
// 5. RCExpiredDelegation.
// -----------------------------------------------------------------------------

// RCExpiredDelegation mirrors rc_expired_delegation_object: at most one
// row per delegator, holding the residual RC that couldn't be drained
// during a single-block overflow sweep. New direct delegations from
// `from` are blocked while this row exists.
//
// The C++ object stores only (from, expired_delegation). We carry an
// ExpiryTime so Phase 2's gradual drain hook has a stable handle for
// time-based pacing — strictly additive (the field defaults to 0 if a
// caller doesn't fill it).
type RCExpiredDelegation struct {
	From             auth.AccountName
	RemainingExpired int64
	ExpiryTime       uint32
}

func rcExpiredDelegationKey(from auth.AccountName) []byte {
	return state.AppendAccountName(nil, from)
}

// AppendRCExpiredDelegation serializes d.
func AppendRCExpiredDelegation(dst []byte, d *RCExpiredDelegation) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(d.From)))
	dst = append(dst, d.From...)
	dst = appendI64LE(dst, d.RemainingExpired)
	return appendU32LE(dst, d.ExpiryTime)
}

// ReadRCExpiredDelegation decodes d.
func ReadRCExpiredDelegation(src []byte) (RCExpiredDelegation, error) {
	r := fcraw.NewReader(src)
	var d RCExpiredDelegation
	var err error
	if d.From, err = r.ReadString(); err != nil {
		return d, fmt.Errorf("rc_expired.from: %w", err)
	}
	if d.RemainingExpired, err = r.ReadInt64(); err != nil {
		return d, fmt.Errorf("rc_expired.remaining: %w", err)
	}
	if d.ExpiryTime, err = r.ReadUint32(); err != nil {
		return d, fmt.Errorf("rc_expired.expiry: %w", err)
	}
	if !r.AtEnd() {
		return d, errors.New("rc_expired_delegation: trailing bytes")
	}
	return d, nil
}

// PutRCExpiredDelegation stores d (primary = from; one row per delegator).
func PutRCExpiredDelegation(s *state.Session, d *RCExpiredDelegation) {
	s.Put(RCExpiredDelegationTable, state.PrimaryIndex,
		rcExpiredDelegationKey(d.From), AppendRCExpiredDelegation(nil, d))
}

// GetRCExpiredDelegation fetches the row for `from`.
func GetRCExpiredDelegation(s *state.Session, from auth.AccountName) (RCExpiredDelegation, error) {
	v, err := s.Get(RCExpiredDelegationTable, state.PrimaryIndex, rcExpiredDelegationKey(from))
	if err != nil {
		return RCExpiredDelegation{}, err
	}
	return ReadRCExpiredDelegation(v)
}

// HasRCExpiredDelegation reports whether `from` has an unresolved
// overflow blocking new delegations.
func HasRCExpiredDelegation(s *state.Session, from auth.AccountName) bool {
	has, _ := s.Has(RCExpiredDelegationTable, state.PrimaryIndex, rcExpiredDelegationKey(from))
	return has
}

// DeleteRCExpiredDelegation clears the row when drained.
func DeleteRCExpiredDelegation(s *state.Session, from auth.AccountName) {
	s.Delete(RCExpiredDelegationTable, state.PrimaryIndex, rcExpiredDelegationKey(from))
}

// -----------------------------------------------------------------------------
// 6. RCUsageBucket.
// -----------------------------------------------------------------------------

// RCUsageBucket mirrors rc_usage_bucket_object — one hour of cumulative
// resource consumption. Exactly RCWindowBucketCount of these rotate in
// a ring buffer; the BucketSeqno is a monotonic counter so the by-seqno
// index always identifies the oldest / newest bucket.
type RCUsageBucket struct {
	BucketSeqno uint32
	// Created is the time_point_sec timestamp the bucket was opened
	// (mirrors `timestamp` in the C++ object).
	Created uint32
	Used    [RCNumResources]int64
}

func rcUsageBucketKey(seqno uint32) []byte {
	return state.AppendUint32BE(nil, seqno)
}

// AppendRCUsageBucket serializes b.
func AppendRCUsageBucket(dst []byte, b *RCUsageBucket) []byte {
	dst = appendU32LE(dst, b.BucketSeqno)
	dst = appendU32LE(dst, b.Created)
	for i := 0; i < RCNumResources; i++ {
		dst = appendI64LE(dst, b.Used[i])
	}
	return dst
}

// ReadRCUsageBucket decodes b.
func ReadRCUsageBucket(src []byte) (RCUsageBucket, error) {
	r := fcraw.NewReader(src)
	var b RCUsageBucket
	var err error
	if b.BucketSeqno, err = r.ReadUint32(); err != nil {
		return b, fmt.Errorf("rc_usage_bucket.seqno: %w", err)
	}
	if b.Created, err = r.ReadUint32(); err != nil {
		return b, fmt.Errorf("rc_usage_bucket.created: %w", err)
	}
	for i := 0; i < RCNumResources; i++ {
		if b.Used[i], err = r.ReadInt64(); err != nil {
			return b, fmt.Errorf("rc_usage_bucket.used[%d]: %w", i, err)
		}
	}
	if !r.AtEnd() {
		return b, errors.New("rc_usage_bucket: trailing bytes")
	}
	return b, nil
}

// PutRCUsageBucket stores b and maintains the by-seqno index.
func PutRCUsageBucket(s *state.Session, b *RCUsageBucket) {
	s.Put(RCUsageBucketTable, state.PrimaryIndex,
		rcUsageBucketKey(b.BucketSeqno), AppendRCUsageBucket(nil, b))
	s.Put(RCUsageBucketTable, RCUsageBucketBySeqnoIndex,
		rcUsageBucketKey(b.BucketSeqno), nil)
}

// GetRCUsageBucket fetches the bucket by seqno.
func GetRCUsageBucket(s *state.Session, seqno uint32) (RCUsageBucket, error) {
	v, err := s.Get(RCUsageBucketTable, state.PrimaryIndex, rcUsageBucketKey(seqno))
	if err != nil {
		return RCUsageBucket{}, err
	}
	return ReadRCUsageBucket(v)
}

// DeleteRCUsageBucket removes the bucket and its index entry.
func DeleteRCUsageBucket(s *state.Session, seqno uint32) {
	s.Delete(RCUsageBucketTable, state.PrimaryIndex, rcUsageBucketKey(seqno))
	s.Delete(RCUsageBucketTable, RCUsageBucketBySeqnoIndex, rcUsageBucketKey(seqno))
}

// -----------------------------------------------------------------------------
// Local serialization helpers (uint32/uint16/int32 LE — others reused
// from dgpo.go / witness.go).
// -----------------------------------------------------------------------------

func appendI32LE(dst []byte, v int32) []byte {
	return append(dst,
		byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}
