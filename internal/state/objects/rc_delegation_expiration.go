package objects

import (
	"errors"
	"fmt"

	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

// RCDelegationExpirationTable holds rc_delegation_expiration_object rows
// — pending RC returns from a delegator who reduced their delegation.
// The reduced amount is held for HIVE_RC_DELEGATION_RETURN_PERIOD (5 days)
// before crediting back to the delegator's available RC.
//
// Mirrors hive::chain::rc_delegation_expiration_object.
const RCDelegationExpirationTable state.TableID = 29

// RCDelegationExpirationByExpirationIndex orders by (expiration, id) for
// the per-block prune sweep. Key: [expiration_BE_4][seq_BE_8].
const RCDelegationExpirationByExpirationIndex state.IndexID = 1

// RCDelegationExpiration is one pending-return row.
type RCDelegationExpiration struct {
	// Seq is a monotonic counter assigned at creation; used as the
	// primary key so multiple expirations from the same delegator can
	// coexist.
	Seq        uint64
	Delegator  auth.AccountName
	RC         uint64
	Expiration uint32
}

func rcDelegationExpKey(seq uint64) []byte {
	return state.AppendUint64BE(nil, seq)
}

func rcDelegationExpByExpirationKey(expiration uint32, seq uint64) []byte {
	out := state.AppendUint32BE(nil, expiration)
	return state.AppendUint64BE(out, seq)
}

// AppendRCDelegationExpiration serializes d.
func AppendRCDelegationExpiration(dst []byte, d *RCDelegationExpiration) []byte {
	dst = appendU64LE(dst, d.Seq)
	dst = fcraw.AppendVarUint(dst, uint32(len(d.Delegator)))
	dst = append(dst, d.Delegator...)
	dst = appendU64LE(dst, d.RC)
	dst = appendU32LE(dst, d.Expiration)
	return dst
}

// ReadRCDelegationExpiration decodes d.
func ReadRCDelegationExpiration(src []byte) (RCDelegationExpiration, error) {
	r := fcraw.NewReader(src)
	var d RCDelegationExpiration
	var err error
	if d.Seq, err = r.ReadUint64(); err != nil {
		return d, fmt.Errorf("rc_delegation_expiration.seq: %w", err)
	}
	if d.Delegator, err = r.ReadString(); err != nil {
		return d, fmt.Errorf("rc_delegation_expiration.delegator: %w", err)
	}
	if d.RC, err = r.ReadUint64(); err != nil {
		return d, fmt.Errorf("rc_delegation_expiration.rc: %w", err)
	}
	if d.Expiration, err = r.ReadUint32(); err != nil {
		return d, fmt.Errorf("rc_delegation_expiration.expiration: %w", err)
	}
	if !r.AtEnd() {
		return d, errors.New("rc_delegation_expiration: trailing bytes")
	}
	return d, nil
}

// PutRCDelegationExpiration stores d and maintains the by-expiration index.
func PutRCDelegationExpiration(s *state.Session, d *RCDelegationExpiration) {
	s.Put(RCDelegationExpirationTable, state.PrimaryIndex,
		rcDelegationExpKey(d.Seq), AppendRCDelegationExpiration(nil, d))
	s.Put(RCDelegationExpirationTable, RCDelegationExpirationByExpirationIndex,
		rcDelegationExpByExpirationKey(d.Expiration, d.Seq), nil)
}

// GetRCDelegationExpiration fetches a single row by its primary seq id.
func GetRCDelegationExpiration(s *state.Session, seq uint64) (RCDelegationExpiration, error) {
	v, err := s.Get(RCDelegationExpirationTable, state.PrimaryIndex, rcDelegationExpKey(seq))
	if err != nil {
		return RCDelegationExpiration{}, err
	}
	return ReadRCDelegationExpiration(v)
}

// DeleteRCDelegationExpiration removes both the primary row and the
// secondary index entry.
func DeleteRCDelegationExpiration(s *state.Session, d *RCDelegationExpiration) {
	s.Delete(RCDelegationExpirationTable, state.PrimaryIndex, rcDelegationExpKey(d.Seq))
	s.Delete(RCDelegationExpirationTable, RCDelegationExpirationByExpirationIndex,
		rcDelegationExpByExpirationKey(d.Expiration, d.Seq))
}
