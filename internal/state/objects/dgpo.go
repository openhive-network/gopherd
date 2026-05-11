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
	// HIVE backing the VESTS pool (the "vesting fund"). Vesting price =
	// TotalVestingShares / TotalVestingFundHive (with 1e-6 / 1e-3 precision
	// scaling baked in by virtue of the satoshi-level division). When
	// either is 0, the initial 1:1000 satoshi-level ratio is used.
	TotalVestingFundHive int64
	// Reward pool that comment cashouts draw from. process_funds adds the
	// per-block content_reward here; processCommentCashouts subtracts the
	// author + curator slice as comments pay out.
	TotalRewardFundHive int64
	// Sum of evaluate_reward_curve(c.NetRshares) over all unpaid comments
	// — hived's `dynamic_global_property_object::total_reward_shares2`.
	// Stored as 16 bytes big-endian (uint128). Vote evaluators
	// adjust_rshares2(old_curve, new_curve) on every cast/recast; the
	// cashout subtracts the comment's curve value.
	TotalRewardShares2 [16]byte
	// HBD interest rate (basis points × 100; HIVE_DEFAULT_HBD_INTEREST_RATE
	// = 1000 = 10% APR). The witness schedule object publishes the median
	// of active witnesses; we shadow the median into DGPO each block when
	// the schedule rebuilds. Until that lands, the rate stays at the
	// genesis default.
	HBDInterestRate uint16
	// HBDPrintRate (HF14+) — basis points of HIVE_100_PERCENT (10000 ==
	// 100%). Cashout HBD payouts multiply by this rate; the rest goes
	// out as HIVE. update_virtual_supply ramps it down once HBD's share
	// of the virtual supply rises past hbd_start_percent.
	HBDPrintRate uint16
	// HBDStartPercent / HBDStopPercent (HF14, HF20, HF26) — basis points.
	// Below start: print 100%. Above stop: print 0%. Linear in between.
	HBDStartPercent uint16
	HBDStopPercent  uint16
	// VirtualSupply = current_supply + (current_hbd_supply × median_price).
	// Recomputed each block in update_virtual_supply.
	VirtualSupply int64
	// Inflation / reward split percentages. Pre-HF16 these are unused
	// (process_funds adds the floor amounts directly); HF16+ uses these
	// to slice new_hive into content / vesting / proposal / witness
	// rewards.
	ContentRewardPercent uint16
	VestingRewardPercent uint16
	ProposalFundPercent  uint16
	// ReverseAuctionSeconds — curation reward time penalty window
	// (libraries/protocol/include/hive/protocol/config.hpp:
	//   HIVE_REVERSE_AUCTION_WINDOW_SECONDS_HF6 = 1800
	//   HIVE_REVERSE_AUCTION_WINDOW_SECONDS_HF20 = 900
	//   HIVE_REVERSE_AUCTION_WINDOW_SECONDS_HF21 = 300).
	// Votes within this window after creation get penalized weight.
	ReverseAuctionSeconds uint32
	// DelegationReturnPeriod (HF20+) — seconds after a delegation
	// reduction that the VESTS return to the delegator. Pre-HF20 it's
	// HIVE_VOTING_MANA_REGENERATION_SECONDS (5 days); HF20 sets the
	// same value, HF26 may shorten further. Stored so per-block hooks
	// can read it without re-deriving from HF state.
	DelegationReturnPeriod uint32
	// AvailableAccountSubsidies (HF20+) — leaky-bucket pool that
	// claim_account draws from. Per-block hook increments by
	// account_subsidy_budget and decays by account_subsidy_decay × pool.
	// claim_account requires the pool ≥ HIVE_ACCOUNT_SUBSIDY_PRECISION
	// (10000) to issue a free claim instead of charging the fee.
	// Mirrors dynamic_global_property_object::available_account_subsidies.
	AvailableAccountSubsidies int64
	// NextCommentID is a monotonic counter assigned to each newly-created
	// comment so the cashout schedule can tie-break same-cashout_time
	// entries by insertion order — matching C++'s
	// composite_key<cashout_time, comment_id> on by_cashout_time. Without
	// this, alphabetical (author, permlink) order disagrees with hived
	// at the HF7 cascade and propagates fund/shares2 drift through every
	// subsequent cashout.
	NextCommentID uint64
	// NumPowWitnesses counts pow operations applied so far (mirrors
	// dynamic_global_property_object::num_pow_witnesses). Pre-HIVE_START_
	// VESTING_BLOCK (block 201,600) get_pow_reward returns 0 until this
	// counter reaches HIVE_MAX_WITNESSES (21) — see database.cpp:1940.
	// Without this gate, every pow op pre-201,600 pays 21K HIVE-satoshi
	// to the block producer, inflating cur_supply by ~420K vs hived
	// (1.6% drift visible at the HF7 cashout cascade).
	NumPowWitnesses uint32
	// TotalPow is a monotonic counter incremented on every pow / pow2
	// op (mirrors dynamic_global_property_object::total_pow). The
	// receiving witness's pow_worker is set to this value, giving each
	// pow op a UNIQUE pow_worker even when multiple pow ops land in
	// the same block. Without this (using e.g. blockNum as pow_worker),
	// same-block pow ops collide and by_pow ordering diverges from
	// hived's sequence — which feeds back into miner-slot picks at
	// schedule rebuilds and skews the timeshare iter's stop point.
	TotalPow uint64
	// NextLimitOrderID is a monotonic counter assigned to each newly-
	// created limit_order_object. Used as the by_price index tie-break
	// — same-price orders match in insertion order, mirroring C++'s
	// composite_key<sell_price desc, id asc>. Without this, ties break
	// by (owner, user_orderid) — different match sequences than hived,
	// drifts account balances over time.
	NextLimitOrderID uint64
	// LastIrreversibleBlockNum is the chain-wide irreversibility height.
	// Recomputed each block from the median of the active witness
	// schedule's last_confirmed_block_num values (75% threshold). Mirrors
	// dynamic_global_property_object::last_irreversible_block_num.
	LastIrreversibleBlockNum uint32
	// DownvotePoolPercent (HF21+) — basis points (× HIVE_1_PERCENT) of
	// the post reward pool routed to a separate downvote_reward pool.
	// Default HIVE_DOWNVOTE_POOL_PERCENT_HF21 = 25 × HIVE_1_PERCENT = 2500.
	// Mirrors dynamic_global_property_object::downvote_pool_percent
	// (libraries/chain/include/hive/chain/global_property_object.hpp:160).
	DownvotePoolPercent uint16
	// DHFIntervalLedger (HF21+) — running tally of HBD minted to the
	// treasury per process_funds tick (mirror of
	// dynamic_global_property_object::dhf_interval_ledger). Reset by
	// process_proposals at each interval boundary; emitted as the
	// dhf_funding_operation virtual op.
	DHFIntervalLedger int64
	// CurrentAslot is the absolute slot index since genesis, incremented
	// per slot — INCLUDING missed slots — not just per produced block.
	// Mirrors dynamic_global_property_object::current_aslot
	// (libraries/chain/include/hive/chain/global_property_object.hpp:78).
	// Used by `get_scheduled_witness( slot_num )` =
	// `current_shuffled_witnesses[(current_aslot + slot_num) % num_scheduled]`.
	// Without this, Go's `ScheduledWitnessForSlot` falls back on
	// HeadBlockNumber, which DOESN'T advance on missed slots — causing
	// `update_witness_missed_blocks` to lookup the WRONG witness for
	// each missed slot, dramatically undercounting per-witness
	// total_missed (gxt-1080-sc-0028 Go missed=1 vs hived=2 at block
	// 5,140,000 caused HF14+ disable to fire in hived but not Go,
	// which propagated into divergent schedules and the +1 NumPow
	// cascade past block 5,148,675).
	CurrentAslot uint64
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
	dst = appendI64LE(dst, p.TotalVestingFundHive)
	dst = appendI64LE(dst, p.TotalRewardFundHive)
	dst = append(dst, p.TotalRewardShares2[:]...)
	dst = append(dst, byte(p.HBDInterestRate), byte(p.HBDInterestRate>>8))
	dst = append(dst, byte(p.HBDPrintRate), byte(p.HBDPrintRate>>8))
	dst = append(dst, byte(p.HBDStartPercent), byte(p.HBDStartPercent>>8))
	dst = append(dst, byte(p.HBDStopPercent), byte(p.HBDStopPercent>>8))
	dst = appendI64LE(dst, p.VirtualSupply)
	dst = append(dst, byte(p.ContentRewardPercent), byte(p.ContentRewardPercent>>8))
	dst = append(dst, byte(p.VestingRewardPercent), byte(p.VestingRewardPercent>>8))
	dst = append(dst, byte(p.ProposalFundPercent), byte(p.ProposalFundPercent>>8))
	dst = appendU32LE(dst, p.ReverseAuctionSeconds)
	dst = appendU32LE(dst, p.DelegationReturnPeriod)
	dst = appendI64LE(dst, p.AvailableAccountSubsidies)
	dst = appendU64LE(dst, p.NextCommentID)
	dst = appendU32LE(dst, p.NumPowWitnesses)
	dst = appendU64LE(dst, p.NextLimitOrderID)
	dst = appendU32LE(dst, p.LastIrreversibleBlockNum)
	dst = append(dst, byte(p.DownvotePoolPercent), byte(p.DownvotePoolPercent>>8))
	dst = appendU64LE(dst, p.TotalPow)
	dst = appendI64LE(dst, p.DHFIntervalLedger)
	dst = appendU64LE(dst, p.CurrentAslot)
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
	if p.TotalVestingFundHive, err = r.ReadInt64(); err != nil {
		return p, fmt.Errorf("dgpo.total_vesting_fund_hive: %w", err)
	}
	// total_reward_fund_hive is a later addition; tolerate older rows
	// (which won't have it) by treating EOF as zero.
	if r.AtEnd() {
		return p, nil
	}
	if p.TotalRewardFundHive, err = r.ReadInt64(); err != nil {
		return p, fmt.Errorf("dgpo.total_reward_fund_hive: %w", err)
	}
	// total_reward_shares2 is a later addition.
	if r.AtEnd() {
		return p, nil
	}
	if err := r.ReadInto(p.TotalRewardShares2[:]); err != nil {
		return p, fmt.Errorf("dgpo.total_reward_shares2: %w", err)
	}
	if r.AtEnd() {
		return p, nil
	}
	if p.HBDInterestRate, err = r.ReadUint16(); err != nil {
		return p, fmt.Errorf("dgpo.hbd_interest_rate: %w", err)
	}
	// HF14+/HF16+ extension fields land here. Tolerate EOF for older
	// rows seeded before this schema bump.
	if r.AtEnd() {
		return p, nil
	}
	if p.HBDPrintRate, err = r.ReadUint16(); err != nil {
		return p, err
	}
	if p.HBDStartPercent, err = r.ReadUint16(); err != nil {
		return p, err
	}
	if p.HBDStopPercent, err = r.ReadUint16(); err != nil {
		return p, err
	}
	if p.VirtualSupply, err = r.ReadInt64(); err != nil {
		return p, err
	}
	if p.ContentRewardPercent, err = r.ReadUint16(); err != nil {
		return p, err
	}
	if p.VestingRewardPercent, err = r.ReadUint16(); err != nil {
		return p, err
	}
	if p.ProposalFundPercent, err = r.ReadUint16(); err != nil {
		return p, err
	}
	if p.ReverseAuctionSeconds, err = r.ReadUint32(); err != nil {
		return p, err
	}
	if p.DelegationReturnPeriod, err = r.ReadUint32(); err != nil {
		return p, err
	}
	// AvailableAccountSubsidies is a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return p, nil
	}
	if p.AvailableAccountSubsidies, err = r.ReadInt64(); err != nil {
		return p, fmt.Errorf("dgpo.available_account_subsidies: %w", err)
	}
	// NextCommentID is a later schema addition; tolerate EOF for legacy rows.
	if r.AtEnd() {
		return p, nil
	}
	if p.NextCommentID, err = r.ReadUint64(); err != nil {
		return p, fmt.Errorf("dgpo.next_comment_id: %w", err)
	}
	if r.AtEnd() {
		return p, nil
	}
	if p.NumPowWitnesses, err = r.ReadUint32(); err != nil {
		return p, fmt.Errorf("dgpo.num_pow_witnesses: %w", err)
	}
	if r.AtEnd() {
		return p, nil
	}
	if p.NextLimitOrderID, err = r.ReadUint64(); err != nil {
		return p, fmt.Errorf("dgpo.next_limit_order_id: %w", err)
	}
	// LastIrreversibleBlockNum is a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return p, nil
	}
	if p.LastIrreversibleBlockNum, err = r.ReadUint32(); err != nil {
		return p, fmt.Errorf("dgpo.last_irreversible_block_num: %w", err)
	}
	// DownvotePoolPercent is a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return p, nil
	}
	if p.DownvotePoolPercent, err = r.ReadUint16(); err != nil {
		return p, fmt.Errorf("dgpo.downvote_pool_percent: %w", err)
	}
	// TotalPow is a later schema addition; tolerate EOF for legacy rows.
	if r.AtEnd() {
		return p, nil
	}
	if p.TotalPow, err = r.ReadUint64(); err != nil {
		return p, fmt.Errorf("dgpo.total_pow: %w", err)
	}
	// DHFIntervalLedger is a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return p, nil
	}
	if p.DHFIntervalLedger, err = r.ReadInt64(); err != nil {
		return p, fmt.Errorf("dgpo.dhf_interval_ledger: %w", err)
	}
	// CurrentAslot is a later schema addition; tolerate EOF for legacy rows.
	if r.AtEnd() {
		return p, nil
	}
	if p.CurrentAslot, err = r.ReadUint64(); err != nil {
		return p, fmt.Errorf("dgpo.current_aslot: %w", err)
	}
	if !r.AtEnd() {
		return p, errors.New("dgpo: trailing bytes after decode (schema mismatch?)")
	}
	return p, nil
}


// PutDGPO writes the singleton via the typed DGPOStore. No K/V Put —
// the previous serialize+overlay path was the largest residual
// Session.Put alloc source (~1.7 GB / 4M blocks for ~5 PutDGPO calls
// per block). Restore-on-tx-failure is handled by the subsystem's
// Snapshot/Restore machinery (DGPOStore.RestoreToPoint).
func PutDGPO(s *state.Session, p *DynamicGlobalProperty) {
	EnsureDGPOStore(s).Put(p)
}

// GetDGPO fetches the singleton.
func GetDGPO(s *state.Session) (DynamicGlobalProperty, error) {
	p, ok := EnsureDGPOStore(s).Get()
	if !ok {
		return DynamicGlobalProperty{}, state.ErrNotFound
	}
	return p, nil
}

// --- private helpers ----------------------------------------------------

func appendU32LE(dst []byte, v uint32) []byte {
	return binary.LittleEndian.AppendUint32(dst, v)
}

func appendI64LE(dst []byte, v int64) []byte {
	return binary.LittleEndian.AppendUint64(dst, uint64(v))
}

func appendU64LE(dst []byte, v uint64) []byte {
	return binary.LittleEndian.AppendUint64(dst, v)
}
