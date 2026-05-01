package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// RCDirectDelegationTable holds direct RC delegations from one account
// to another. Mirrors hive::chain::rc_direct_delegation_object.
const RCDirectDelegationTable state.TableID = 23

// RCDirectDelegation is one (from, to, delegated_rc) row.
type RCDirectDelegation struct {
	From         auth.AccountName
	To           auth.AccountName
	DelegatedRC  uint64
}

// rcDirectDelegationKey: (from, to) — at most one delegation per pair.
func rcDirectDelegationKey(from, to auth.AccountName) []byte {
	out := state.AppendAccountName(nil, from)
	return state.AppendAccountName(out, to)
}

// AppendRCDirectDelegation serializes d.
func AppendRCDirectDelegation(dst []byte, d *RCDirectDelegation) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(d.From)))
	dst = append(dst, d.From...)
	dst = fcraw.AppendVarUint(dst, uint32(len(d.To)))
	dst = append(dst, d.To...)
	return appendU64LE(dst, d.DelegatedRC)
}

// ReadRCDirectDelegation decodes a row.
func ReadRCDirectDelegation(src []byte) (RCDirectDelegation, error) {
	r := fcraw.NewReader(src)
	var d RCDirectDelegation
	from, err := r.ReadString()
	if err != nil {
		return d, fmt.Errorf("rcd.from: %w", err)
	}
	d.From = from
	to, err := r.ReadString()
	if err != nil {
		return d, fmt.Errorf("rcd.to: %w", err)
	}
	d.To = to
	if d.DelegatedRC, err = r.ReadUint64(); err != nil {
		return d, fmt.Errorf("rcd.delegated_rc: %w", err)
	}
	if !r.AtEnd() {
		return d, errors.New("rc_direct_delegation: trailing bytes")
	}
	return d, nil
}

// PutRCDirectDelegation stores d.
func PutRCDirectDelegation(s *state.Session, d *RCDirectDelegation) {
	s.Put(RCDirectDelegationTable, state.PrimaryIndex,
		rcDirectDelegationKey(d.From, d.To), AppendRCDirectDelegation(nil, d))
}

// GetRCDirectDelegation fetches the (from, to) delegation, returning
// ErrNotFound if absent.
func GetRCDirectDelegation(s *state.Session, from, to auth.AccountName) (RCDirectDelegation, error) {
	v, err := s.Get(RCDirectDelegationTable, state.PrimaryIndex, rcDirectDelegationKey(from, to))
	if err != nil {
		return RCDirectDelegation{}, err
	}
	return ReadRCDirectDelegation(v)
}

// DeleteRCDirectDelegation removes the (from, to) delegation.
func DeleteRCDirectDelegation(s *state.Session, from, to auth.AccountName) {
	s.Delete(RCDirectDelegationTable, state.PrimaryIndex, rcDirectDelegationKey(from, to))
}
