package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// VestingDelegationExpirationTable holds rows scheduling the return of
// VESTS to a delegator after a delegation is reduced. Created by
// delegate_vesting_shares when delta < 0; processed in a per-block
// hook that returns matured rows' VESTS to the delegator.
//
// Mirrors libraries/chain/include/hive/chain/detail/state/vesting_delegation_expiration_object_multiindex.hpp.
const VestingDelegationExpirationTable state.TableID = 22

// VestingDelegationExpirationByDateIndex sorts rows by expiration_BE so
// the per-block processor can stop scanning once an unmatured row is
// reached.
const VestingDelegationExpirationByDateIndex state.IndexID = 1

// VestingDelegationExpiration mirrors hived's
// vesting_delegation_expiration_object: an obligation to return
// `vesting_shares` VESTS to `delegator` after `expiration` has passed.
type VestingDelegationExpiration struct {
	ID            uint32
	Delegator     auth.AccountName
	VestingShares int64 // VESTS-satoshi
	Expiration    uint32
}

// Primary key: (delegator, ID) so multiple expirations from the same
// account coexist.
func vestingDelegationExpirationKey(delegator auth.AccountName, id uint32) []byte {
	out := state.AppendAccountName(nil, delegator)
	return state.AppendUint32BE(out, id)
}

// vestingDelegationExpirationByDateKey: (expiration_BE, delegator, ID)
// for chronological iteration.
func vestingDelegationExpirationByDateKey(expiration uint32, delegator auth.AccountName, id uint32) []byte {
	out := state.AppendUint32BE(nil, expiration)
	out = state.AppendAccountName(out, delegator)
	return state.AppendUint32BE(out, id)
}

// AppendVestingDelegationExpiration serializes a row.
func AppendVestingDelegationExpiration(dst []byte, e *VestingDelegationExpiration) []byte {
	dst = appendU32LE(dst, e.ID)
	dst = fcraw.AppendVarUint(dst, uint32(len(e.Delegator)))
	dst = append(dst, e.Delegator...)
	dst = appendI64LE(dst, e.VestingShares)
	dst = appendU32LE(dst, e.Expiration)
	return dst
}

// ReadVestingDelegationExpiration decodes a row.
func ReadVestingDelegationExpiration(src []byte) (VestingDelegationExpiration, error) {
	r := fcraw.NewReader(src)
	var e VestingDelegationExpiration
	var err error
	if e.ID, err = r.ReadUint32(); err != nil {
		return e, fmt.Errorf("vde.id: %w", err)
	}
	if e.Delegator, err = r.ReadString(); err != nil {
		return e, fmt.Errorf("vde.delegator: %w", err)
	}
	if e.VestingShares, err = r.ReadInt64(); err != nil {
		return e, fmt.Errorf("vde.vesting_shares: %w", err)
	}
	if e.Expiration, err = r.ReadUint32(); err != nil {
		return e, fmt.Errorf("vde.expiration: %w", err)
	}
	if !r.AtEnd() {
		return e, errors.New("vde: trailing bytes")
	}
	return e, nil
}

// PutVestingDelegationExpiration stores e and maintains the by-date
// secondary index.
func PutVestingDelegationExpiration(s *state.Session, e *VestingDelegationExpiration) {
	s.Put(VestingDelegationExpirationTable, state.PrimaryIndex,
		vestingDelegationExpirationKey(e.Delegator, e.ID),
		AppendVestingDelegationExpiration(nil, e))
	s.Put(VestingDelegationExpirationTable, VestingDelegationExpirationByDateIndex,
		vestingDelegationExpirationByDateKey(e.Expiration, e.Delegator, e.ID), nil)
	if e.Expiration != 0 {
		s.NudgeExpHint(VestingDelegationExpirationTable, VestingDelegationExpirationByDateIndex, e.Expiration)
	}
}

// DeleteVestingDelegationExpirationFull removes both index rows for e.
func DeleteVestingDelegationExpirationFull(s *state.Session, e *VestingDelegationExpiration) {
	s.Delete(VestingDelegationExpirationTable, state.PrimaryIndex,
		vestingDelegationExpirationKey(e.Delegator, e.ID))
	s.Delete(VestingDelegationExpirationTable, VestingDelegationExpirationByDateIndex,
		vestingDelegationExpirationByDateKey(e.Expiration, e.Delegator, e.ID))
}
