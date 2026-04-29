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
	// Pending reward balances (claim_reward_balance moves these into the
	// liquid Balance/HBDBalance/VestingShares fields).
	RewardHive  int64
	RewardHBD   int64
	RewardVests int64
	// MemoKey is the 33-byte compressed secp256k1 memo key.
	MemoKey [33]byte
	// JSONMetadata is the account's published metadata blob.
	JSONMetadata string
	// Proxy is the account_witness_proxy target. Empty string ("" or
	// HIVE_PROXY_TO_SELF_ACCOUNT in C++) means no proxy.
	Proxy auth.AccountName
	// WitnessesVotedFor counts how many witnesses this account has voted
	// for directly. Capped at HIVE_MAX_ACCOUNT_WITNESS_VOTES (30).
	WitnessesVotedFor uint16
	// PendingClaimedAccounts is the count of paid-for-but-not-yet-used
	// account claims (HF20+). claim_account increments;
	// create_claimed_account decrements.
	PendingClaimedAccounts uint16
	// SavingsBalance / SavingsHBDBalance hold funds in the time-locked
	// savings account.
	SavingsBalance    int64
	SavingsHBDBalance int64
	// DelegatedVestingShares are VESTS this account has delegated out
	// to others; ReceivedVestingShares are VESTS delegated in.
	DelegatedVestingShares int64
	ReceivedVestingShares  int64
	// Vesting power-down schedule (withdraw_vesting). Pays
	// HIVE_VESTING_WITHDRAW_INTERVAL_SECONDS apart for
	// HIVE_VESTING_WITHDRAW_INTERVALS iterations.
	VestingWithdrawRate   int64
	NextVestingWithdrawal uint32
	ToWithdraw            int64
	Withdrawn             int64
}

// AppendAccount serializes a to dst using FC binary form. The field
// order chosen here is internal to the state schema — it does not need
// to match hived's account_object FC_REFLECT — but it MUST be preserved
// across releases of the schema.
func AppendAccount(dst []byte, a *Account) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(a.Name)))
	dst = append(dst, a.Name...)
	dst = appendU32LE(dst, a.Created)
	dst = appendI64LE(dst, a.Balance)
	dst = appendI64LE(dst, a.HBDBalance)
	dst = appendI64LE(dst, a.VestingShares)
	dst = appendI64LE(dst, a.RewardHive)
	dst = appendI64LE(dst, a.RewardHBD)
	dst = appendI64LE(dst, a.RewardVests)
	dst = append(dst, a.MemoKey[:]...)
	dst = fcraw.AppendVarUint(dst, uint32(len(a.JSONMetadata)))
	dst = append(dst, a.JSONMetadata...)
	dst = fcraw.AppendVarUint(dst, uint32(len(a.Proxy)))
	dst = append(dst, a.Proxy...)
	dst = append(dst, byte(a.WitnessesVotedFor), byte(a.WitnessesVotedFor>>8))
	dst = append(dst, byte(a.PendingClaimedAccounts), byte(a.PendingClaimedAccounts>>8))
	dst = appendI64LE(dst, a.SavingsBalance)
	dst = appendI64LE(dst, a.SavingsHBDBalance)
	dst = appendI64LE(dst, a.DelegatedVestingShares)
	dst = appendI64LE(dst, a.ReceivedVestingShares)
	dst = appendI64LE(dst, a.VestingWithdrawRate)
	dst = appendU32LE(dst, a.NextVestingWithdrawal)
	dst = appendI64LE(dst, a.ToWithdraw)
	dst = appendI64LE(dst, a.Withdrawn)
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
	if a.RewardHive, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.reward_hive: %w", err)
	}
	if a.RewardHBD, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.reward_hbd: %w", err)
	}
	if a.RewardVests, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.reward_vests: %w", err)
	}
	if err := r.ReadInto(a.MemoKey[:]); err != nil {
		return a, fmt.Errorf("account.memo_key: %w", err)
	}
	if a.JSONMetadata, err = r.ReadString(); err != nil {
		return a, fmt.Errorf("account.json_metadata: %w", err)
	}
	if a.Proxy, err = r.ReadString(); err != nil {
		return a, fmt.Errorf("account.proxy: %w", err)
	}
	if a.WitnessesVotedFor, err = r.ReadUint16(); err != nil {
		return a, fmt.Errorf("account.witnesses_voted_for: %w", err)
	}
	if a.PendingClaimedAccounts, err = r.ReadUint16(); err != nil {
		return a, fmt.Errorf("account.pending_claimed_accounts: %w", err)
	}
	if a.SavingsBalance, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.savings_balance: %w", err)
	}
	if a.SavingsHBDBalance, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.savings_hbd_balance: %w", err)
	}
	if a.DelegatedVestingShares, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.delegated_vesting_shares: %w", err)
	}
	if a.ReceivedVestingShares, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.received_vesting_shares: %w", err)
	}
	if a.VestingWithdrawRate, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.vesting_withdraw_rate: %w", err)
	}
	if a.NextVestingWithdrawal, err = r.ReadUint32(); err != nil {
		return a, fmt.Errorf("account.next_vesting_withdrawal: %w", err)
	}
	if a.ToWithdraw, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.to_withdraw: %w", err)
	}
	if a.Withdrawn, err = r.ReadInt64(); err != nil {
		return a, fmt.Errorf("account.withdrawn: %w", err)
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
