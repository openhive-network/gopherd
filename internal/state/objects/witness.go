package objects

import (
	"errors"
	"fmt"
	"math/big"

	"hived-go/internal/crypto"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
)

// Witness corresponds to hive::chain::witness_object — minimal-viable
// fields tracked here. Hived's full schema also includes the witness
// schedule slot, recent missed/produced counts, the running version
// vote, hardfork_version_vote, and so on; those land alongside the
// witness scheduler.
type Witness struct {
	Owner                  auth.AccountName
	Created                uint32
	URL                    string
	BlockSigningKey        crypto.PublicKey
	AccountCreationFee     int64 // legacy_chain_properties.account_creation_fee.amount
	MaximumBlockSize       uint32
	HBDInterestRate        uint16
	TotalVotesVests        int64 // sum of voter vesting_shares
	TotalMissedBlocks      uint32
	HardforkVersionVote    uint32 // packed v_num; 0 = unset
	HardforkTimeVote       uint32 // time_point_sec
	RunningVersion         uint32
	// HBDExchangeRate is the most recent feed_publish from this witness
	// — a price of HBD/HIVE. Zero amount on Base means "not set yet".
	HBDExchangeRate         asset.Price
	LastHBDExchangeUpdate   uint32 // time_point_sec of last feed_publish
	// AvailableWitnessAccountSubsidies — per-witness subsidy pool that
	// claim_account can draw from BEFORE the global pool. Mirrors
	// witness_object::available_witness_account_subsidies. Updated
	// every block by process_subsidized_accounts based on
	// account_subsidy_witness_rd parameters.
	AvailableWitnessAccountSubsidies int64
	// Schedule — which slot type this witness occupies in the active
	// schedule (witness_object::schedule_type in witness_objects.hpp).
	// Updated every shuffle by update_witness_schedule. process_funds
	// reads this to apply the per-slot pay weight.
	Schedule WitnessScheduleType
	// VirtualLastUpdate / VirtualPosition / VirtualScheduledTime are the
	// uint128 trio (16 bytes each, big-endian wire format) used by
	// hived's update_witness_schedule_4 to pick the timeshare slot.
	// VirtualScheduledTime is the chain-wide rank ordering: smaller
	// value = sooner-to-be-scheduled. After being picked as timeshare,
	// the witness's VST advances by HIVE_VIRTUAL_SCHEDULE_LAP_LENGTH2
	// / (votes + 1), so high-vote witnesses get re-scheduled sooner.
	// Mirrors witness_object fields virtual_last_update,
	// virtual_position, virtual_scheduled_time.
	VirtualLastUpdate    [16]byte
	VirtualPosition      [16]byte
	VirtualScheduledTime [16]byte
	// PowWorker is a counter set by pow / pow2 ops to put the witness
	// in hived's `by_pow` index. update_witness_schedule_4 pops the
	// smallest-PowWorker witness as the miner slot and zeroes their
	// PowWorker (so the by_pow index drops them). pre-HF13 only.
	// Mirrors witness_object::pow_worker.
	PowWorker uint64
	// Account subsidy parameters voted by this witness via
	// witness_set_properties — mirror chain_properties::account_subsidy_budget
	// and account_subsidy_decay (libraries/protocol/include/hive/protocol/
	// chain_properties.hpp). Used by update_median_witness_props at HF20+
	// to seed the account_subsidy_witness_rd parameters and the global
	// account_subsidy_rd. Defaults match HIVE_DEFAULT_ACCOUNT_SUBSIDY_*
	// constants (797 / 347321).
	AccountSubsidyBudget int32
	AccountSubsidyDecay  uint32
	// LastConfirmedBlockNum — the block number this witness most recently
	// produced. Used by update_last_irreversible_block_num to derive the
	// chain-wide LIB from the median of the active schedule's
	// last_confirmed values. Mirrors witness_object::last_confirmed_block_num
	// (libraries/chain/include/hive/chain/witness_objects.hpp).
	LastConfirmedBlockNum uint32
	// LastWork — pow1 work hash (32-byte sha256) submitted by this
	// witness. Hived's pow_apply (hive_evaluator.cpp:407-413) rejects
	// any pow1 op whose work hash matches an existing witness's
	// last_work. Without a global by_work index, Go enforces the
	// same-witness case only (witness X resubmitting their own proof
	// after a drain) — cross-witness collision requires a SHA256
	// collision and is astronomically rare. pow2 does NOT touch this
	// field (C++ pow2_evaluator never writes last_work).
	LastWork [32]byte
	// dirtyEpoch is a transient (NOT serialized) tag the WitnessStore
	// uses to dedup the per-round dirty queue. Equal to
	// WitnessStore.dirtyEpoch ⇒ this Witness is already in dirtyOrder
	// for the current round. Skipping a per-Mutate map.access on this
	// hot path. See WitnessStore.markDirty / flushSecondaries.
	dirtyEpoch uint32
}

// WitnessScheduleType mirrors witness_object::schedule_type:
//
//	none      = 0 (default, not in active schedule)
//	elected   = 1 (vote-elected slot)
//	timeshare = 2 (low-vote rotating slot)
//	miner     = 3 (PoW slot, deprecated post-HF13)
type WitnessScheduleType uint8

const (
	WitnessScheduleNone      WitnessScheduleType = 0
	WitnessScheduleElected   WitnessScheduleType = 1
	WitnessScheduleTimeshare WitnessScheduleType = 2
	WitnessScheduleMiner     WitnessScheduleType = 3
)

// AccountWitnessVote represents one (account, witness) edge in the
// witness vote graph. Stored under WitnessVoteTable with a composite
// primary key (account_name || witness_name).
type AccountWitnessVote struct {
	Account auth.AccountName
	Witness auth.AccountName
}

// Table IDs.
const (
	WitnessTable      state.TableID = 2
	WitnessVoteTable  state.TableID = 3
	WitnessVotesByOwnerIndex state.IndexID = 1 // secondary: owner -> witness names voted
	// WitnessByVoteIndex orders witnesses by total_votes_vests DESC,
	// owner ASC. Used by update_witness_schedule to pick the top 21
	// without scanning the whole witness table. Key:
	// [^uint64(votes)_BE_8][owner_padded_16].
	WitnessByVoteIndex state.IndexID = 2
	// WitnessByScheduleTimeIndex orders witnesses by VirtualScheduledTime
	// ASC, owner ASC. Mirrors hived's by_schedule_time multi_index used
	// by update_witness_schedule_4 to pick the timeshare slot
	// (smallest virtual_scheduled_time wins). Key: [vst_BE_16][owner_padded_16].
	WitnessByScheduleTimeIndex state.IndexID = 3
	// WitnessByPowIndex orders witnesses by PowWorker ASC (excluding
	// witnesses with PowWorker == 0). Mirrors hived's by_pow
	// multi_index. Key: [pow_BE_8][owner_padded_16].
	WitnessByPowIndex state.IndexID = 4
)

// witnessByVoteKey encodes (votes, owner) so byte-wise lex order
// matches descending votes, ascending owner. Votes are inverted via
// MaxUint64 - uint64(votes) (votes are always >= 0 in practice).
func witnessByVoteKey(votes int64, owner auth.AccountName) []byte {
	v := uint64(votes)
	if votes < 0 {
		v = 0
	}
	out := state.AppendUint64BE(nil, ^v)
	return state.AppendAccountName(out, owner)
}

// witnessByScheduleTimeKey encodes (virtual_scheduled_time, owner) so
// lex order matches ascending VST then ascending owner. VST is stored
// big-endian so byte-wise compare matches uint128 numeric compare.
func witnessByScheduleTimeKey(vst [16]byte, owner auth.AccountName) []byte {
	out := append(make([]byte, 0, 32), vst[:]...)
	return state.AppendAccountName(out, owner)
}

// witnessByPowKey encodes (pow_worker, owner). PowWorker == 0 marks
// "not in the pow queue" — callers must skip the index entry for those.
func witnessByPowKey(pow uint64, owner auth.AccountName) []byte {
	out := state.AppendUint64BE(nil, pow)
	return state.AppendAccountName(out, owner)
}

// AppendWitness serializes w to dst.
func AppendWitness(dst []byte, w *Witness) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(w.Owner)))
	dst = append(dst, w.Owner...)
	dst = appendU32LE(dst, w.Created)
	dst = fcraw.AppendVarUint(dst, uint32(len(w.URL)))
	dst = append(dst, w.URL...)
	dst = append(dst, w.BlockSigningKey[:]...)
	dst = appendI64LE(dst, w.AccountCreationFee)
	dst = appendU32LE(dst, w.MaximumBlockSize)
	dst = appendU16LE(dst, w.HBDInterestRate)
	dst = appendI64LE(dst, w.TotalVotesVests)
	dst = appendU32LE(dst, w.TotalMissedBlocks)
	dst = appendU32LE(dst, w.HardforkVersionVote)
	dst = appendU32LE(dst, w.HardforkTimeVote)
	dst = appendU32LE(dst, w.RunningVersion)
	dst = asset.AppendPrice(dst, w.HBDExchangeRate)
	dst = appendU32LE(dst, w.LastHBDExchangeUpdate)
	dst = appendI64LE(dst, w.AvailableWitnessAccountSubsidies)
	dst = append(dst, byte(w.Schedule))
	dst = append(dst, w.VirtualLastUpdate[:]...)
	dst = append(dst, w.VirtualPosition[:]...)
	dst = append(dst, w.VirtualScheduledTime[:]...)
	dst = appendU64LE(dst, w.PowWorker)
	dst = appendU32LE(dst, uint32(w.AccountSubsidyBudget))
	dst = appendU32LE(dst, w.AccountSubsidyDecay)
	dst = appendU32LE(dst, w.LastConfirmedBlockNum)
	dst = append(dst, w.LastWork[:]...)
	return dst
}

// ReadWitness decodes a witness from src.
func ReadWitness(src []byte) (Witness, error) {
	r := fcraw.NewReader(src)
	var w Witness
	owner, err := r.ReadString()
	if err != nil {
		return w, fmt.Errorf("witness.owner: %w", err)
	}
	w.Owner = owner
	if w.Created, err = r.ReadUint32(); err != nil {
		return w, fmt.Errorf("witness.created: %w", err)
	}
	if w.URL, err = r.ReadString(); err != nil {
		return w, fmt.Errorf("witness.url: %w", err)
	}
	if err := r.ReadInto(w.BlockSigningKey[:]); err != nil {
		return w, fmt.Errorf("witness.signing_key: %w", err)
	}
	if w.AccountCreationFee, err = r.ReadInt64(); err != nil {
		return w, err
	}
	if w.MaximumBlockSize, err = r.ReadUint32(); err != nil {
		return w, err
	}
	if w.HBDInterestRate, err = r.ReadUint16(); err != nil {
		return w, err
	}
	if w.TotalVotesVests, err = r.ReadInt64(); err != nil {
		return w, err
	}
	if w.TotalMissedBlocks, err = r.ReadUint32(); err != nil {
		return w, err
	}
	if w.HardforkVersionVote, err = r.ReadUint32(); err != nil {
		return w, err
	}
	if w.HardforkTimeVote, err = r.ReadUint32(); err != nil {
		return w, err
	}
	if w.RunningVersion, err = r.ReadUint32(); err != nil {
		return w, err
	}
	// HBDExchangeRate / LastHBDExchangeUpdate are a later schema
	// addition; treat EOF as "not set yet" so legacy rows still decode.
	if r.AtEnd() {
		return w, nil
	}
	if w.HBDExchangeRate, err = asset.ReadPrice(r); err != nil {
		return w, fmt.Errorf("witness.hbd_exchange_rate: %w", err)
	}
	if w.LastHBDExchangeUpdate, err = r.ReadUint32(); err != nil {
		return w, fmt.Errorf("witness.last_hbd_exchange_update: %w", err)
	}
	// AvailableWitnessAccountSubsidies is a later schema addition;
	// tolerate EOF for legacy rows.
	if r.AtEnd() {
		return w, nil
	}
	if w.AvailableWitnessAccountSubsidies, err = r.ReadInt64(); err != nil {
		return w, fmt.Errorf("witness.available_witness_account_subsidies: %w", err)
	}
	// Schedule is a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return w, nil
	}
	sb, err := r.ReadByte()
	if err != nil {
		return w, fmt.Errorf("witness.schedule: %w", err)
	}
	w.Schedule = WitnessScheduleType(sb)
	// VirtualLastUpdate / VirtualPosition / VirtualScheduledTime / PowWorker
	// are a later schema addition — tolerate EOF for older rows.
	if r.AtEnd() {
		return w, nil
	}
	if err := r.ReadInto(w.VirtualLastUpdate[:]); err != nil {
		return w, fmt.Errorf("witness.virtual_last_update: %w", err)
	}
	if err := r.ReadInto(w.VirtualPosition[:]); err != nil {
		return w, fmt.Errorf("witness.virtual_position: %w", err)
	}
	if err := r.ReadInto(w.VirtualScheduledTime[:]); err != nil {
		return w, fmt.Errorf("witness.virtual_scheduled_time: %w", err)
	}
	if w.PowWorker, err = r.ReadUint64(); err != nil {
		return w, fmt.Errorf("witness.pow_worker: %w", err)
	}
	// AccountSubsidyBudget / AccountSubsidyDecay are a later schema
	// addition; tolerate EOF for older rows.
	if r.AtEnd() {
		return w, nil
	}
	asb, err := r.ReadUint32()
	if err != nil {
		return w, fmt.Errorf("witness.account_subsidy_budget: %w", err)
	}
	w.AccountSubsidyBudget = int32(asb)
	if w.AccountSubsidyDecay, err = r.ReadUint32(); err != nil {
		return w, fmt.Errorf("witness.account_subsidy_decay: %w", err)
	}
	// LastConfirmedBlockNum is a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return w, nil
	}
	if w.LastConfirmedBlockNum, err = r.ReadUint32(); err != nil {
		return w, fmt.Errorf("witness.last_confirmed_block_num: %w", err)
	}
	// LastWork is a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return w, nil
	}
	if err := r.ReadInto(w.LastWork[:]); err != nil {
		return w, fmt.Errorf("witness.last_work: %w", err)
	}
	if !r.AtEnd() {
		return w, errors.New("witness: trailing bytes")
	}
	return w, nil
}

// WitnessKey is the primary-index key bytes for a witness named owner.
func WitnessKey(owner auth.AccountName) []byte {
	return state.AppendAccountName(nil, owner)
}

// PutWitnessIndexed is the fast-path variant of PutWitness used when the
// caller already has the pre-modify Witness values in hand. With the
// chainbase-style WitnessStore the prior values aren't strictly needed
// (the store knows the prior pointer internally), so this is a thin
// wrapper for backward compatibility.
func PutWitnessIndexed(s *state.Session, w *Witness, _ int64, _ [16]byte, _ uint64) {
	EnsureWitnessStore(s).Put(w)
}

// PutWitness stores w under its owner key and maintains the by-vote,
// by-schedule-time, and by-pow secondary indexes via the typed
// WitnessStore.
func PutWitness(s *state.Session, w *Witness) {
	EnsureWitnessStore(s).Put(w)
}

// virtualTimeIsZero reports whether all 16 bytes are 0 — meaning the
// witness has no by_schedule_time index entry yet (typical at row
// creation; first set when reset_virtual_schedule_time runs or when
// update_witness_schedule_4 advances them).
func virtualTimeIsZero(v [16]byte) bool {
	for _, b := range v {
		if b != 0 {
			return false
		}
	}
	return true
}

// GetWitness fetches the witness named owner from the typed store.
func GetWitness(s *state.Session, owner auth.AccountName) (Witness, error) {
	w, ok := EnsureWitnessStore(s).Get(owner)
	if !ok {
		return Witness{}, state.ErrNotFound
	}
	return w, nil
}

// GetWitnessRef returns a read-only pointer to the witness slot for
// owner. Skips the ~hundreds-of-bytes Witness value-copy that
// GetWitness pays. Aliases internal storage — see WitnessStore.GetRef
// docs for the read-only contract.
func GetWitnessRef(s *state.Session, owner auth.AccountName) (*Witness, bool) {
	return EnsureWitnessStore(s).GetRef(owner)
}

// MutateWitness applies an in-place modification to the witness slot
// for owner via WitnessStore.Mutate. Returns false if the row
// doesn't exist. Saves the Get value-copy + Put copy-back pair on
// the schedule-rebuild hot path (21 witnesses × ~190K rounds = ~4M
// 256-byte copies eliminated per 4M-block run).
func MutateWitness(s *state.Session, owner auth.AccountName, fn func(*Witness)) bool {
	return EnsureWitnessStore(s).Mutate(owner, fn)
}

// HasWitness reports whether a witness owner exists.
func HasWitness(s *state.Session, owner auth.AccountName) (bool, error) {
	return EnsureWitnessStore(s).Has(owner), nil
}

// FlushWitnessIndexes is a no-op in the typed-store world — the
// WitnessStore keeps secondary indexes synchronously consistent with
// the primary map. Kept for API stability with callers that still
// invoke it before a Range.
func FlushWitnessIndexes(_ *state.Session) {}

// PutWitnessVote records that account votes for witness.
func PutWitnessVote(s *state.Session, account, witness auth.AccountName) {
	EnsureWitnessVoteStore(s).Put(account, witness)
}

// HasWitnessVote reports whether account currently votes for witness.
func HasWitnessVote(s *state.Session, account, witness auth.AccountName) (bool, error) {
	return EnsureWitnessVoteStore(s).Has(account, witness), nil
}

// DeleteWitnessVote removes account's vote for witness.
func DeleteWitnessVote(s *state.Session, account, witness auth.AccountName) {
	EnsureWitnessVoteStore(s).Delete(account, witness)
}

// AdjustWitnessVotesForVoter walks every witness_vote row cast by
// `account` and shifts each voted-for witness's total_votes_vests by
// `delta`. Mirrors hived's database::adjust_proxied_witness_votes flat
// path (the no-proxy case in
// libraries/chain/database.cpp:1108-1129) — every time a voter's
// vesting weight changes (transfer_to_vesting, withdraw chunk fired,
// delegate_vesting_shares delta), the witnesses they vote for need
// their total_votes_vests re-weighted by the same delta.
//
// We omit the recursive proxy chain re-adjustment (proxied_vsf_votes
// array) since we don't model that field — for accounts with a proxy,
// we credit the proxy target as if it were the direct voter, which
// approximates the hived semantics one level deep.
func AdjustWitnessVotesForVoter(s *state.Session, account auth.AccountName, delta int64) {
	if delta == 0 {
		return
	}
	wvs := EnsureWitnessVoteStore(s)
	if wvs.VotesForCount(account) == 0 {
		return
	}
	dgpo, _ := GetDGPO(s)
	hf := hardfork.SetForBlock(dgpo.HeadBlockNumber)
	// Hoist the witness schedule read out of the per-witness loop —
	// AdjustWitnessVote needs CurrentVirtualTime which doesn't change
	// across iterations of this loop (only updated by the per-block
	// schedule rebuild). Saves a btree Get + ReadWitnessSchedule per
	// witness vote (30+ saves per producer-reward block).
	ws, wsErr := GetWitnessSchedule(s)
	wstore := EnsureWitnessStore(s)
	currentVT := ws.CurrentVirtualTime
	// VotesForEachWitness pre-resolves the *Witness pointer from the
	// vote cache so the inner loop skips one map lookup per witness
	// (30+ per call × every CreateVesting × every block past HF7).
	wvs.VotesForEachWitness(account, wstore, func(witnessName auth.AccountName, w *Witness) bool {
		// Snap capture for tx-rollback safety + dirty-mark, then in-
		// place mutate. Same semantics as wstore.Mutate but the
		// caller already has *Witness.
		if wstore.db.UndoEnabled {
			wstore.undoLog = append(wstore.undoLog, witnessUndo{
				owner: string(witnessName), hadPrev: true, slot: w, snap: *w,
			})
		}
		if wsErr == nil {
			adjustWitnessVoteWith(w, delta, hf, currentVT)
		} else {
			w.TotalVotesVests += delta
		}
		wstore.markDirty(w)
		return true
	})
}

// AdjustWitnessVote mirrors C++ database::adjust_witness_vote
// (database_witness.cpp:69) — applies `delta` to w.TotalVotesVests and
// also advances the virtual-time bookkeeping that the timeshare scheduler
// (update_witness_schedule_4) reads:
//
//	delta_pos = old_votes * (current_virtual_time - virtual_last_update)
//	virtual_position += delta_pos
//	virtual_last_update = current_virtual_time
//	votes += delta
//	virtual_scheduled_time = virtual_last_update + (LAP - virtual_position)/(votes+1)
//	HF4+: if virtual_scheduled_time < current_virtual_time → set to MAX
//
// Without this, by_schedule_time becomes stale and timeshare picks
// drift from C++ even when total vesting matches.
func AdjustWitnessVote(s *state.Session, w *Witness, delta int64, hf hardfork.Set) {
	ws, err := GetWitnessSchedule(s)
	if err != nil {
		// No schedule yet (genesis) — fall back to plain vote bump.
		// No negative clamp; C++'s adjust_witness_vote also allows
		// w.votes to go negative.
		w.TotalVotesVests += delta
		return
	}
	adjustWitnessVoteWith(w, delta, hf, ws.CurrentVirtualTime)
}

// adjustWitnessVoteWith is the inner loop callable when the witness schedule's
// CurrentVirtualTime is already in hand — used by AdjustWitnessVotesForVoter
// to hoist the GetWitnessSchedule out of the per-witness loop (called 30+
// times per producer-reward block; each GetWitnessSchedule does a btree Get
// + ReadWitnessSchedule).
func adjustWitnessVoteWith(w *Witness, delta int64, hf hardfork.Set, currentVT16 [16]byte) {
	// Native uint128 path — replaces the previous big.Int implementation
	// which allocated 7+ heap big.Ints per call. Each AdjustWitnessVote
	// fires once per witness in the producer's vote list (≤30) every
	// block via processProducerReward → CreateVesting → AdjustProxiedWitnessVotes.
	currentVT := UnpackUint128BE(currentVT16)
	virtualLastUpdate := UnpackUint128BE(w.VirtualLastUpdate)
	virtualPosition := UnpackUint128BE(w.VirtualPosition)

	// delta_pos = OLD votes * (current_vt - virtual_last_update).
	// virtual_position += delta_pos — wraps mod 2^128 (C++ uint128_t
	// native overflow; Uint128.Add wraps the same way).
	//
	// w.TotalVotesVests can be negative (matching C++ safe<int64>; see
	// applyWitnessVoteDelta — pre-HF2 unvote+revote nets correctly only
	// without a clamp). When negative, C++ promotes int64 → uint128 via
	// SIGN-EXTENSION (uint128(int64): hi = -(l<0), lo = l), so
	//   delta_pos = (sign_ext_votes) * dt  ≡  -|votes| * dt  (mod 2^128).
	// Naive Go `dt.Mul64(uint64(int64))` only sign-extends to 64 bits, so
	// the high half of the would-be sign extension (= dt * 2^64) leaks
	// into virtual_position and immediately blows it past LAP_LENGTH
	// (2^64-1). After that, (LAP - virtual_position) underflows to ~2^128
	// and VST = currentVT + ~2^128/votes ≈ 5e22 (constant LAP2/votes),
	// pinning the witness at the END of by_schedule_time so it's never
	// re-picked as timeshare — root of the iter5/iter17 cascade tail
	// observed at blocks 906,012–1,041,516 (steem-id never picked, fall-
	// back picks like neonminer drive newVT 6× hived's CVT growth rate).
	// Compute |votes| × dt, then add or subtract by sign — equivalent
	// mod 2^128 to C++'s sign-extended multiplication.
	// No `currentVT > VLU` guard: C++ does plain uint128 subtraction
	// even when current_virtual_time < virtual_last_update — which DOES
	// happen when update_witness_schedule_4's timeshare pick advances
	// new_virtual_time to a picked witness's OLD VST that's smaller
	// than the chain-wide CVT was a moment ago. In that case dt
	// underflows to ~2^128, delta_pos = votes × wrapped_dt mod 2^128
	// is a near-2^128 (i.e. effectively negative-mod-2^128) value, and
	// vp += delta_pos wraps to a value just below LAP_LENGTH2. The
	// next VST formula then gives a small (LAP - vp) → small advance
	// → small VST. Concrete repro: at block 959,112 bytemaster unvoted
	// adm; hived's CVT had regressed by 273 since adm's last touch, so
	// hived's vp ended at 2^128 - 6.67e16 and adm.VST stabilized at
	// 6.67e16 (well below CVT, so adm becomes a near-front timeshare
	// candidate). Our guard skipped the wrap, leaving vp=0 and VST≈CVT,
	// flipping the by_VST ordering for the next 20K rounds.
	dt := currentVT.Sub(virtualLastUpdate) // wraps if VLU > CVT
	votes := w.TotalVotesVests
	if votes >= 0 {
		dp := dt.Mul64(uint64(votes))
		virtualPosition = virtualPosition.Add(dp)
	} else {
		dp := dt.Mul64(uint64(-votes))
		virtualPosition = virtualPosition.Sub(dp)
	}
	currentVT.PackBE16Into(&w.VirtualLastUpdate)

	// No negative clamp — C++'s adjust_witness_vote (database_witness.cpp:79)
	// only asserts `w.votes <= total_vesting_shares` (upper bound). Allowing
	// negatives is required so unvote+revote at pre-HF2 nets correctly
	// instead of inflating the witness's vote total by the voter's full vest.
	w.TotalVotesVests += delta
	virtualPosition.PackBE16Into(&w.VirtualPosition)

	// Lap length: HF2+ uses LAP_LENGTH2 (2^128-1); pre-HF2 uses
	// LAP_LENGTH (2^64-1).
	var lap Uint128
	if hf.Has(hardfork.HF2) {
		lap = Uint128Max // 2^128 - 1
	} else {
		lap = Uint128{Hi: 0, Lo: ^uint64(0)} // 2^64 - 1
	}
	// virtual_scheduled_time = virtual_last_update + (lap - virtual_position) / (votes + 1).
	// In C++ this is uint128 subtraction — when virtual_position > lap
	// it wraps to a huge value, the divide gives a huge advance, then
	// VST overflows past current_vt and the HF4+ clamp catches it.
	advance := lap.Sub(virtualPosition) // wraps if vp > lap
	// (votes+1) needs C++-style sign-extension to uint128: positive →
	// uint128(0, lo); negative → uint128(max, lo). Naive `uint64(int64)`
	// only sign-extends to 64 bits, so a negative votes+1 yields a much
	// smaller divisor than C++'s, inflating the advance during the
	// transient negative-votes window of pre-HF2 unvote+revote.
	votesPlus1 := w.TotalVotesVests + 1
	var divisor Uint128
	if votesPlus1 >= 0 {
		divisor = Uint128{Hi: 0, Lo: uint64(votesPlus1)}
	} else {
		// Sign-extend: hi = -1 (all bits set), lo = 2's-complement of
		// the int64 reinterpreted as uint64.
		divisor = Uint128{Hi: ^uint64(0), Lo: uint64(votesPlus1)}
	}
	advance = advance.Quo(divisor)
	newVST := currentVT.Add(advance) // wraps mod 2^128
	// HF4+: if VST < current_vt (mod-128 overflow), clamp to uint128 max.
	if hf.Has(hardfork.HF4) {
		if newVST.Cmp(currentVT) < 0 {
			newVST = Uint128Max
		}
	}
	newVST.PackBE16Into(&w.VirtualScheduledTime)
}

// packBig128 returns the 16-byte big-endian uint128 representation of v.
// Caller is responsible for keeping v in the [0, 2^128) range — values
// outside the range are silently truncated to the low 128 bits. Uses
// FillBytes (Go 1.15+) to avoid the per-call alloc that v.Bytes() does.
func packBig128(v *big.Int) [16]byte {
	var out [16]byte
	if v == nil || v.Sign() <= 0 {
		return out
	}
	if v.BitLen() > 128 {
		// Mask to low 128 bits via temporary; fall back to slow path.
		src := v.Bytes()
		src = src[len(src)-16:]
		copy(out[:], src)
		return out
	}
	v.FillBytes(out[:])
	return out
}

// uint128 helper constants — package-level immutable big.Ints to avoid
// allocating fresh big.Ints on every AdjustWitnessVote call (alloc
// profile showed math/big.nat.make at 18% of allocations driven by
// these helpers). Treat the returned pointer as read-only — never pass
// to a mutating method (Add, Sub, etc.) as the LEFT operand.
var (
	uint128ModulusVal = new(big.Int).Lsh(big.NewInt(1), 128)
	lapLength2Val     = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	lapLengthVal      = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1))
)

// uint128Modulus returns 2^128 — the modulus for wrapping uint128 math.
// Used to keep virtual_position and virtual_scheduled_time in
// [0, 2^128) like C++'s native uint128 wraparound.
func uint128Modulus() *big.Int { return uint128ModulusVal }

// lapLength2 returns 2^128 - 1 (HF2+ lap length).
func lapLength2() *big.Int { return lapLength2Val }

// lapLength returns 2^64 - 1 (pre-HF2 lap length).
func lapLength() *big.Int { return lapLengthVal }

// lap128Max returns 2^128 - 1 — sentinel for HF4+ overflow clamp.
func lap128Max() *big.Int { return lapLength2Val }

// ClearWitnessVotesForAccount walks every witness_vote row keyed under
// account and removes it, decrementing each affected witness's
// total_votes_vests by voterVesting. Mirrors hived's
// database::clear_witness_votes (database_witness.cpp:96), invoked by
// account_witness_proxy when a proxy is set and by
// process_decline_voting_rights.
//
// When voterVesting > 0, mutations go through AdjustWitnessVote so the
// virtual_position / virtual_scheduled_time are updated alongside
// total_votes_vests — matching C++'s adjust_witness_vote() path used by
// HF22+ governance-vote-expiration sweeps and process_decline_voting_rights.
//
// Returns the number of votes cleared.
func ClearWitnessVotesForAccount(s *state.Session, account auth.AccountName, voterVesting int64) int {
	votes := EnsureWitnessVoteStore(s).VotesFor(account)
	for _, witness := range votes {
		DeleteWitnessVote(s, account, witness)
		if voterVesting == 0 {
			continue
		}
		w, err := GetWitness(s, witness)
		if err == nil {
			// Use AdjustWitnessVote so the virtual_position + VST
			// re-roll matches every other vote-decrement path.
			AdjustWitnessVote(s, &w, -voterVesting, hardforkSetForBlockNum(s))
		}
	}
	return len(votes)
}

// hardforkSetForBlockNum returns the active hardfork set at the
// session's current head block. Stored as a function indirection so
// state/objects doesn't have to import the hardfork package at build
// time — caller injects via the session's DGPO snapshot.
func hardforkSetForBlockNum(s *state.Session) hardfork.Set {
	dgpo, err := GetDGPO(s)
	if err != nil {
		return 0
	}
	return hardfork.SetForBlock(dgpo.HeadBlockNumber)
}

// --- private helpers (added here to avoid cycling imports) --------------

func appendU16LE(dst []byte, v uint16) []byte {
	return append(dst, byte(v), byte(v>>8))
}
