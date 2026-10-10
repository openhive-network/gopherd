// Package objects — RC subsystem Phase 5 pending-data singleton.
//
// Mirrors hive::plugins::rc::rc_pending_data in
// libraries/plugins/rc/include/hive/plugins/rc/rc_objects.hpp.
//
// The pending-data singleton accumulates two things across a block:
//
//   * Per-block: pending_usage + pending_cost (summed per transaction).
//     These are flushed into the current rc_usage_bucket's Used array
//     at the per-block finalize hook, then reset for the next block.
//
//   * Per-tx: differential_usage — the (negative) resource delta computed
//     when a state-shrinking operation will replace an existing object.
//     This is reset at the pre-apply tx hook, accumulated by per-op
//     differential visitors, and rolled into the tx's count_resources
//     output at the post-apply tx hook.
//
// Pre-HF20 the table is empty (no rows). Phase 5 init creates a single
// row at the HF20 activation boundary.

package objects

import (
	"errors"
	"fmt"

	"gopherd/internal/fcraw"
	"gopherd/internal/state"
)

// RCPendingDataTable holds the singleton rc_pending_data row.
const RCPendingDataTable state.TableID = 34

// rcPendingDataKey is the singleton-key sentinel.
var rcPendingDataKey = []byte{0}

// RCPendingData mirrors rc_pending_data.
//
// TxCount counts the transactions accumulated since the last per-block
// reset. PendingUsage / PendingCost are the running totals — usage is
// in raw resource units (history_bytes…execution_time), cost is in RC.
//
// DifferentialUsage carries the per-tx (negative) state-bytes delta
// computed by the per-op differential visitor. It is reset on every
// pre-apply tx hook and added into the count_resources output before
// the cost is charged.
type RCPendingData struct {
	TxCount           uint32
	PendingUsage      [RCNumResources]int64
	PendingCost       [RCNumResources]int64
	DifferentialUsage [RCNumResources]int64
}

// AppendRCPendingData serializes d.
func AppendRCPendingData(dst []byte, d *RCPendingData) []byte {
	dst = appendU32LE(dst, d.TxCount)
	for i := 0; i < RCNumResources; i++ {
		dst = appendI64LE(dst, d.PendingUsage[i])
	}
	for i := 0; i < RCNumResources; i++ {
		dst = appendI64LE(dst, d.PendingCost[i])
	}
	for i := 0; i < RCNumResources; i++ {
		dst = appendI64LE(dst, d.DifferentialUsage[i])
	}
	return dst
}

// ReadRCPendingData decodes d.
func ReadRCPendingData(src []byte) (RCPendingData, error) {
	r := fcraw.NewReader(src)
	var d RCPendingData
	var err error
	if d.TxCount, err = r.ReadUint32(); err != nil {
		return d, fmt.Errorf("rc_pending.tx_count: %w", err)
	}
	for i := 0; i < RCNumResources; i++ {
		if d.PendingUsage[i], err = r.ReadInt64(); err != nil {
			return d, fmt.Errorf("rc_pending.pending_usage[%d]: %w", i, err)
		}
	}
	for i := 0; i < RCNumResources; i++ {
		if d.PendingCost[i], err = r.ReadInt64(); err != nil {
			return d, fmt.Errorf("rc_pending.pending_cost[%d]: %w", i, err)
		}
	}
	for i := 0; i < RCNumResources; i++ {
		if d.DifferentialUsage[i], err = r.ReadInt64(); err != nil {
			return d, fmt.Errorf("rc_pending.differential_usage[%d]: %w", i, err)
		}
	}
	if !r.AtEnd() {
		return d, errors.New("rc_pending_data: trailing bytes")
	}
	return d, nil
}

// PutRCPendingData stores the singleton.
func PutRCPendingData(s *state.Session, d *RCPendingData) {
	// Written once per transaction from HF20 on. Encoding into a nil slice
	// grew it through five doublings each time — 10% of all bytes allocated
	// during a mainnet sync — so reuse the Session's serialization scratch,
	// which Put copies out of.
	s.SerScratch = AppendRCPendingData(s.SerScratch[:0], d)
	s.Put(RCPendingDataTable, state.PrimaryIndex, rcPendingDataKey, s.SerScratch)
}

// GetRCPendingData fetches the singleton, returning a zero value if it
// doesn't exist yet (singleton is created lazily on first access).
func GetRCPendingData(s *state.Session) RCPendingData {
	v, err := s.Get(RCPendingDataTable, state.PrimaryIndex, rcPendingDataKey)
	if err != nil {
		return RCPendingData{}
	}
	d, err := ReadRCPendingData(v)
	if err != nil {
		return RCPendingData{}
	}
	return d
}

// HasRCPendingData reports existence.
func HasRCPendingData(s *state.Session) bool {
	has, _ := s.Has(RCPendingDataTable, state.PrimaryIndex, rcPendingDataKey)
	return has
}

// ResetDifferentialUsage clears the per-tx differential accumulator.
// Called from the pre-apply tx hook.
func (d *RCPendingData) ResetDifferentialUsage() {
	for i := 0; i < RCNumResources; i++ {
		d.DifferentialUsage[i] = 0
	}
}

// AddDifferentialUsage accumulates the (negative of the) supplied usage
// into the differential accumulator. Mirrors C++
// rc_pending_data::add_differential_usage.
func (d *RCPendingData) AddDifferentialUsage(usage [RCNumResources]int64) {
	for i := 0; i < RCNumResources; i++ {
		d.DifferentialUsage[i] -= usage[i]
	}
}

// ResetPendingUsage clears the per-block pending totals + tx counter.
// Called from the pre-apply block hook.
func (d *RCPendingData) ResetPendingUsage() {
	d.TxCount = 0
	for i := 0; i < RCNumResources; i++ {
		d.PendingUsage[i] = 0
		d.PendingCost[i] = 0
	}
}

// AddPendingUsage accumulates one tx's (usage, cost) into the per-block
// totals. Mirrors C++ rc_pending_data::add_pending_usage.
func (d *RCPendingData) AddPendingUsage(usage, cost [RCNumResources]int64) {
	d.TxCount++
	for i := 0; i < RCNumResources; i++ {
		d.PendingUsage[i] += usage[i]
		d.PendingCost[i] += cost[i]
	}
}
