package objects

import (
	"errors"
	"fmt"

	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

// OwnerAuthorityHistoryTable holds rows recorded every time an account
// changes its owner authority. Mirrors hive::chain::owner_authority_history_object.
//
// Used by the recovery flow: an attacker who steals the account's keys
// can replace the owner authority, but for HIVE_OWNER_UPDATE_LIMIT
// (30 days) the previous owner can request_account_recovery using a
// key from THIS history (a "previous_owner_authority" within the
// recovery window).
const OwnerAuthorityHistoryTable state.TableID = 36 // was 22, shared with VestingDelegationExpirationTable (same 20-byte key shape)

// OwnerAuthorityHistory is one row of recorded owner-authority change.
type OwnerAuthorityHistory struct {
	Account                auth.AccountName
	PreviousOwnerAuthority auth.Authority
	// LastValidTime is the head_block_time when the change was made.
	// Hived's recovery code accepts this row as proof of ownership
	// within HIVE_OWNER_AUTH_RECOVERY_PERIOD (30 days).
	LastValidTime uint32
}

// ownerAuthorityHistoryKey: composite (account, last_valid_time).
// One account can have multiple history rows over time.
func ownerAuthorityHistoryKey(account auth.AccountName, t uint32) []byte {
	out := state.AppendAccountName(nil, account)
	return state.AppendUint32BE(out, t)
}

// AppendOwnerAuthorityHistory serializes h.
func AppendOwnerAuthorityHistory(dst []byte, h *OwnerAuthorityHistory) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(h.Account)))
	dst = append(dst, h.Account...)
	dst = appendU32LE(dst, h.LastValidTime)
	dst = auth.AppendAuthority(dst, h.PreviousOwnerAuthority)
	return dst
}

// ReadOwnerAuthorityHistory decodes a row.
func ReadOwnerAuthorityHistory(src []byte) (OwnerAuthorityHistory, error) {
	r := fcraw.NewReader(src)
	var h OwnerAuthorityHistory
	name, err := r.ReadString()
	if err != nil {
		return h, fmt.Errorf("oah.account: %w", err)
	}
	h.Account = name
	if h.LastValidTime, err = r.ReadUint32(); err != nil {
		return h, fmt.Errorf("oah.last_valid_time: %w", err)
	}
	a, err := auth.ReadAuthority(r)
	if err != nil {
		return h, fmt.Errorf("oah.previous_owner_authority: %w", err)
	}
	h.PreviousOwnerAuthority = a
	if !r.AtEnd() {
		return h, errors.New("oah: trailing bytes")
	}
	return h, nil
}

// PutOwnerAuthorityHistory stores a row.
func PutOwnerAuthorityHistory(s *state.Session, h *OwnerAuthorityHistory) {
	s.Put(OwnerAuthorityHistoryTable, state.PrimaryIndex,
		ownerAuthorityHistoryKey(h.Account, h.LastValidTime),
		AppendOwnerAuthorityHistory(nil, h))
}

// HasOwnerAuthorityHistoryFor reports whether any history row exists
// for the named account.
func HasOwnerAuthorityHistoryFor(s *state.Session, account auth.AccountName) bool {
	prefix := state.AppendAccountName(nil, account)
	found := false
	_ = s.Range(OwnerAuthorityHistoryTable, state.PrimaryIndex, prefix,
		func(_, _ []byte) (bool, error) {
			found = true
			return false, nil
		})
	return found
}
