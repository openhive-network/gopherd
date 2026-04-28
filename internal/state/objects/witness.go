package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/crypto"
	"hived-go/internal/fcraw"
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
}

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
)

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
	if !r.AtEnd() {
		return w, errors.New("witness: trailing bytes")
	}
	return w, nil
}

// WitnessKey is the primary-index key bytes for a witness named owner.
func WitnessKey(owner auth.AccountName) []byte {
	return state.AppendAccountName(nil, owner)
}

// PutWitness stores w under its owner key.
func PutWitness(s *state.Session, w *Witness) {
	s.Put(WitnessTable, state.PrimaryIndex, WitnessKey(w.Owner), AppendWitness(nil, w))
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

// --- private helpers (added here to avoid cycling imports) --------------

func appendU16LE(dst []byte, v uint16) []byte {
	return append(dst, byte(v), byte(v>>8))
}
