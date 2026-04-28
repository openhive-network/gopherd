// Package objects defines the state-object types stored in the chainbase
// replacement, with their FC binary codecs and TableID assignments.
//
// Note on scope: hived's account_object has 60+ fields covering balances,
// vesting, recovery, RC, voting power, witness votes, post counts, etc.
// This Go port introduces fields incrementally as evaluators need them.
// On-disk state format is implementation-internal (Hive consensus does
// not commit a state-root) so we are free to choose a leaner schema.
package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// AccountTable is the TableID for account_object state.
const AccountTable state.TableID = 1

// Account is the minimal-viable account state object. Fields are added as
// evaluators need them; the FC codec preserves field order so future
// schema extensions are append-only.
type Account struct {
	Name auth.AccountName
	// Created is the time_point_sec at which this account was created.
	Created uint32
	// Balance is the liquid HIVE balance, in satoshis (1e-3 HIVE).
	Balance int64
	// HBDBalance is the liquid HBD balance, in satoshis (1e-3 HBD).
	HBDBalance int64
	// VestingShares is the total VESTS held (1e-6 VESTS).
	VestingShares int64
}

// AppendAccount serializes a to dst using FC binary form. Field order
// matches the AccountFCFields slice and must be preserved.
//
// The field order chosen here is internal to the state schema — it does
// not need to match hived's account_object FC_REFLECT.
func AppendAccount(dst []byte, a *Account) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(a.Name)))
	dst = append(dst, a.Name...)
	dst = appendU32LE(dst, a.Created)
	dst = appendI64LE(dst, a.Balance)
	dst = appendI64LE(dst, a.HBDBalance)
	dst = appendI64LE(dst, a.VestingShares)
	return dst
}

// ReadAccount decodes an account from src.
func ReadAccount(src []byte) (Account, error) {
	r := fcraw.NewReader(src)
	var a Account
	name, err := r.ReadString()
	if err != nil {
		return a, fmt.Errorf("account.name: %w", err)
	}
	a.Name = name
	if a.Created, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.created: %w", err)
	}
	if a.Balance, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.balance: %w", err)
	}
	if a.HBDBalance, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.hbd_balance: %w", err)
	}
	if a.VestingShares, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.vesting_shares: %w", err)
	}
	if !r.AtEnd() {
		return a, errors.New("account: trailing bytes after decode (schema mismatch?)")
	}
	return a, nil
}

// AccountKey returns the primary-index key bytes for an account named name.
// Account names are NUL-padded to 16 bytes (AccountNameKeyLen) so byte-wise
// prefix iteration produces alphabetic order.
func AccountKey(name auth.AccountName) []byte {
	return state.AppendAccountName(nil, name)
}

// PutAccount stores a in the session under the primary key derived from
// a.Name.
func PutAccount(s *state.Session, a *Account) {
	s.Put(AccountTable, state.PrimaryIndex, AccountKey(a.Name), AppendAccount(nil, a))
}

// GetAccount fetches the account named name. Returns ErrNotFound if absent.
func GetAccount(s *state.Session, name auth.AccountName) (Account, error) {
	v, err := s.Get(AccountTable, state.PrimaryIndex, AccountKey(name))
	if err != nil {
		return Account{}, err
	}
	return ReadAccount(v)
}

// HasAccount reports whether an account named name exists.
func HasAccount(s *state.Session, name auth.AccountName) (bool, error) {
	return s.Has(AccountTable, state.PrimaryIndex, AccountKey(name))
}
