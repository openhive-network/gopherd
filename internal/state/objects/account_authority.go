package objects

import (
	"errors"
	"fmt"

	"gopherd/internal/crypto"
	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

// AccountAuthorityTable holds the per-account triple of {owner, active,
// posting} authority structs, plus the timestamps tracking when owner
// was last changed (used by recover_account's 30-day window). Mirrors
// hive::chain::account_authority_object (libraries/chain/include/hive/
// chain/detail/state/account_object.hpp:339-364).
//
// Stored as a separate table from Account so signature verification
// reads only what it needs (authority structs are heavyweight). Keyed
// by account name.
const AccountAuthorityTable state.TableID = 27

// AccountAuthority is one row.
type AccountAuthority struct {
	Account             auth.AccountName
	Owner               auth.Authority
	Active              auth.Authority
	Posting             auth.Authority
	LastOwnerUpdate     uint32 // time_point_sec
	PreviousOwnerUpdate uint32 // time_point_sec
}

func accountAuthorityKey(account auth.AccountName) []byte {
	return state.AppendAccountName(nil, account)
}

func AppendAccountAuthority(dst []byte, a *AccountAuthority) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(a.Account)))
	dst = append(dst, a.Account...)
	dst = auth.AppendAuthority(dst, a.Owner)
	dst = auth.AppendAuthority(dst, a.Active)
	dst = auth.AppendAuthority(dst, a.Posting)
	dst = appendU32LE(dst, a.LastOwnerUpdate)
	dst = appendU32LE(dst, a.PreviousOwnerUpdate)
	return dst
}

func ReadAccountAuthority(src []byte) (AccountAuthority, error) {
	r := fcraw.NewReader(src)
	var a AccountAuthority
	name, err := r.ReadString()
	if err != nil {
		return a, fmt.Errorf("account_authority.account: %w", err)
	}
	a.Account = name
	if a.Owner, err = auth.ReadAuthority(r); err != nil {
		return a, fmt.Errorf("account_authority.owner: %w", err)
	}
	if a.Active, err = auth.ReadAuthority(r); err != nil {
		return a, fmt.Errorf("account_authority.active: %w", err)
	}
	if a.Posting, err = auth.ReadAuthority(r); err != nil {
		return a, fmt.Errorf("account_authority.posting: %w", err)
	}
	if a.LastOwnerUpdate, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account_authority.last_owner_update: %w", err)
	}
	if a.PreviousOwnerUpdate, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account_authority.previous_owner_update: %w", err)
	}
	if !r.AtEnd() {
		return a, errors.New("account_authority: trailing bytes")
	}
	return a, nil
}

// PutAccountAuthority stores the authority row AND maintains the by-key
// index (see key_lookup.go). Maintaining it here rather than at the ~15 call
// sites means no caller can forget it. The extra read of the previous row is
// what lets us retire keys that were just revoked; authority writes only happen
// on account create/update/recover/pow, so this is not a hot path.
func PutAccountAuthority(s *state.Session, a *AccountAuthority) {
	var oldKeys map[crypto.PublicKey]struct{}
	if prev, err := GetAccountAuthority(s, a.Account); err == nil {
		oldKeys = authorityKeySet(&prev)
	}
	reindexKeys(s, a.Account, oldKeys, authorityKeySet(a))
	s.Put(AccountAuthorityTable, state.PrimaryIndex,
		accountAuthorityKey(a.Account), AppendAccountAuthority(nil, a))
}

func GetAccountAuthority(s *state.Session, account auth.AccountName) (AccountAuthority, error) {
	v, err := s.Get(AccountAuthorityTable, state.PrimaryIndex, accountAuthorityKey(account))
	if err != nil {
		return AccountAuthority{}, err
	}
	return ReadAccountAuthority(v)
}

func HasAccountAuthority(s *state.Session, account auth.AccountName) (bool, error) {
	return s.Has(AccountAuthorityTable, state.PrimaryIndex, accountAuthorityKey(account))
}

func DeleteAccountAuthority(s *state.Session, account auth.AccountName) {
	if prev, err := GetAccountAuthority(s, account); err == nil {
		reindexKeys(s, account, authorityKeySet(&prev), nil)
	}
	s.Delete(AccountAuthorityTable, state.PrimaryIndex, accountAuthorityKey(account))
}
