package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/crypto"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
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
	if !r.AtEnd() {
		return w, errors.New("witness: trailing bytes")
	}
	return w, nil
}

// WitnessKey is the primary-index key bytes for a witness named owner.
func WitnessKey(owner auth.AccountName) []byte {
	return state.AppendAccountName(nil, owner)
}

// PutWitness stores w under its owner key and maintains the by-vote,
// by-schedule-time, and by-pow secondary indexes. If a prior record
// exists with different values for the indexed fields, the stale
// entries are removed first.
func PutWitness(s *state.Session, w *Witness) {
	// Read prior row (if any) to know what stale index entries to drop.
	prev, hadPrev := readWitnessIndexFields(s, w.Owner)
	if hadPrev {
		if prev.votes != w.TotalVotesVests {
			s.Delete(WitnessTable, WitnessByVoteIndex, witnessByVoteKey(prev.votes, w.Owner))
			s.Put(WitnessTable, WitnessByVoteIndex, witnessByVoteKey(w.TotalVotesVests, w.Owner), nil)
		}
		if prev.vst != w.VirtualScheduledTime {
			if !virtualTimeIsZero(prev.vst) {
				s.Delete(WitnessTable, WitnessByScheduleTimeIndex, witnessByScheduleTimeKey(prev.vst, w.Owner))
			}
			if !virtualTimeIsZero(w.VirtualScheduledTime) {
				s.Put(WitnessTable, WitnessByScheduleTimeIndex, witnessByScheduleTimeKey(w.VirtualScheduledTime, w.Owner), nil)
			}
		}
		if prev.pow != w.PowWorker {
			if prev.pow != 0 {
				s.Delete(WitnessTable, WitnessByPowIndex, witnessByPowKey(prev.pow, w.Owner))
			}
			if w.PowWorker != 0 {
				s.Put(WitnessTable, WitnessByPowIndex, witnessByPowKey(w.PowWorker, w.Owner), nil)
			}
		}
	} else {
		s.Put(WitnessTable, WitnessByVoteIndex, witnessByVoteKey(w.TotalVotesVests, w.Owner), nil)
		if !virtualTimeIsZero(w.VirtualScheduledTime) {
			s.Put(WitnessTable, WitnessByScheduleTimeIndex, witnessByScheduleTimeKey(w.VirtualScheduledTime, w.Owner), nil)
		}
		if w.PowWorker != 0 {
			s.Put(WitnessTable, WitnessByPowIndex, witnessByPowKey(w.PowWorker, w.Owner), nil)
		}
	}
	s.SerScratch = AppendWitness(s.SerScratch[:0], w)
	s.Put(WitnessTable, state.PrimaryIndex, WitnessKey(w.Owner), s.SerScratch)
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

type witnessIndexFields struct {
	votes int64
	vst   [16]byte
	pow   uint64
}

// readWitnessIndexFields fetches the prior index fields for a witness
// without keeping the whole struct around — used by PutWitness to
// detect stale index entries that need deletion.
func readWitnessIndexFields(s *state.Session, owner auth.AccountName) (witnessIndexFields, bool) {
	v, err := s.Get(WitnessTable, state.PrimaryIndex, WitnessKey(owner))
	if err != nil {
		return witnessIndexFields{}, false
	}
	w, err := ReadWitness(v)
	if err != nil {
		return witnessIndexFields{}, false
	}
	return witnessIndexFields{votes: w.TotalVotesVests, vst: w.VirtualScheduledTime, pow: w.PowWorker}, true
}

// readWitnessVotes returns the stored TotalVotesVests for the named
// witness without decoding the rest of the row. Returns (0, false) if
// the witness does not exist or the row is malformed.
func readWitnessVotes(s *state.Session, owner auth.AccountName) (int64, bool) {
	v, err := s.Get(WitnessTable, state.PrimaryIndex, WitnessKey(owner))
	if err != nil {
		return 0, false
	}
	w, err := ReadWitness(v)
	if err != nil {
		return 0, false
	}
	return w.TotalVotesVests, true
}

// _ keeps readWitnessVotes referenced even if no callsite remains
// after the index-aware PutWitness rewrite — the helper is still
// useful to other code paths.
var _ = readWitnessVotes

// GetWitness fetches the witness named owner.
func GetWitness(s *state.Session, owner auth.AccountName) (Witness, error) {
	v, err := s.Get(WitnessTable, state.PrimaryIndex, WitnessKey(owner))
	if err != nil {
		return Witness{}, err
	}
	return ReadWitness(v)
}

// HasWitness reports whether a witness owner exists.
func HasWitness(s *state.Session, owner auth.AccountName) (bool, error) {
	return s.Has(WitnessTable, state.PrimaryIndex, WitnessKey(owner))
}

// witnessVoteKey encodes (account, witness) as a 32-byte composite.
func witnessVoteKey(account, witness auth.AccountName) []byte {
	out := state.AppendAccountName(nil, account)
	return state.AppendAccountName(out, witness)
}

// PutWitnessVote records that account votes for witness.
func PutWitnessVote(s *state.Session, account, witness auth.AccountName) {
	s.Put(WitnessVoteTable, state.PrimaryIndex, witnessVoteKey(account, witness), nil)
}

// HasWitnessVote reports whether account currently votes for witness.
func HasWitnessVote(s *state.Session, account, witness auth.AccountName) (bool, error) {
	return s.Has(WitnessVoteTable, state.PrimaryIndex, witnessVoteKey(account, witness))
}

// DeleteWitnessVote removes account's vote for witness.
func DeleteWitnessVote(s *state.Session, account, witness auth.AccountName) {
	s.Delete(WitnessVoteTable, state.PrimaryIndex, witnessVoteKey(account, witness))
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
	prefix := state.AppendAccountName(nil, account)
	type pair struct{ witness auth.AccountName }
	var votes []pair
	_ = s.Range(WitnessVoteTable, state.PrimaryIndex, prefix,
		func(body, _ []byte) (bool, error) {
			if len(body) < 2*state.AccountNameKeyLen {
				return true, nil
			}
			witnessBytes := body[state.AccountNameKeyLen : 2*state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && witnessBytes[n-1] == 0 {
				n--
			}
			votes = append(votes, pair{witness: string(witnessBytes[:n])})
			return true, nil
		})
	for _, v := range votes {
		w, err := GetWitness(s, v.witness)
		if err != nil {
			continue
		}
		w.TotalVotesVests += delta
		if w.TotalVotesVests < 0 {
			w.TotalVotesVests = 0
		}
		PutWitness(s, &w)
	}
}

// ClearWitnessVotesForAccount walks every witness_vote row keyed under
// account and removes it, decrementing each affected witness's
// total_votes_vests by voterVesting. Mirrors hived's
// database::clear_witness_votes (database_witness.cpp:96), invoked by
// account_witness_proxy when a proxy is set and by
// process_decline_voting_rights.
//
// Returns the number of votes cleared.
func ClearWitnessVotesForAccount(s *state.Session, account auth.AccountName, voterVesting int64) int {
	prefix := state.AppendAccountName(nil, account)
	type pair struct{ witness auth.AccountName }
	var votes []pair
	_ = s.Range(WitnessVoteTable, state.PrimaryIndex, prefix,
		func(body, _ []byte) (bool, error) {
			// body is the row's primary key minus the [table:index] prefix:
			// account_padded(16) || witness_padded(16). Recover the
			// witness name from the trailing 16 bytes.
			if len(body) < 2*state.AccountNameKeyLen {
				return true, nil
			}
			witnessBytes := body[state.AccountNameKeyLen : 2*state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && witnessBytes[n-1] == 0 {
				n--
			}
			votes = append(votes, pair{witness: string(witnessBytes[:n])})
			return true, nil
		})
	for _, v := range votes {
		DeleteWitnessVote(s, account, v.witness)
		w, err := GetWitness(s, v.witness)
		if err == nil {
			w.TotalVotesVests -= voterVesting
			if w.TotalVotesVests < 0 {
				w.TotalVotesVests = 0
			}
			PutWitness(s, &w)
		}
	}
	return len(votes)
}

// --- private helpers (added here to avoid cycling imports) --------------

func appendU16LE(dst []byte, v uint16) []byte {
	return append(dst, byte(v), byte(v>>8))
}
