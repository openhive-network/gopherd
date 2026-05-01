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
	if !r.AtEnd() {
		return w, errors.New("witness: trailing bytes")
	}
	return w, nil
}

// WitnessKey is the primary-index key bytes for a witness named owner.
func WitnessKey(owner auth.AccountName) []byte {
	return state.AppendAccountName(nil, owner)
}

// PutWitness stores w under its owner key and maintains the by-vote
// secondary index. If a prior record exists with different votes, the
// stale by-vote entry is removed first.
func PutWitness(s *state.Session, w *Witness) {
	if oldVotes, ok := readWitnessVotes(s, w.Owner); ok {
		if oldVotes != w.TotalVotesVests {
			s.Delete(WitnessTable, WitnessByVoteIndex, witnessByVoteKey(oldVotes, w.Owner))
			s.Put(WitnessTable, WitnessByVoteIndex, witnessByVoteKey(w.TotalVotesVests, w.Owner), nil)
		}
	} else {
		s.Put(WitnessTable, WitnessByVoteIndex, witnessByVoteKey(w.TotalVotesVests, w.Owner), nil)
	}
	s.SerScratch = AppendWitness(s.SerScratch[:0], w)
	s.Put(WitnessTable, state.PrimaryIndex, WitnessKey(w.Owner), s.SerScratch)
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
