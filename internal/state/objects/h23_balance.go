package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// H23BalanceTable is the singleton table of HF23 cleared-balance snapshots.
// Mirrors the `h23_balances` map embedded in hived's hardfork_property_object
// (libraries/chain/include/hive/chain/hardfork_property_object.hpp). Hived
// inlines the snapshot inside the singleton; we promote it to its own table
// to keep the serialized HardforkPropertyObject schema unchanged for
// pre-existing replay snapshots.
//
// Each row records the per-account (balance, hbd_balance) pair that was
// drained from the account at the moment clear_account was invoked, so
// restore_accounts can re-credit the same amount at HF24.
const H23BalanceTable state.TableID = 35

// H23Balance is one row of the h23_balances map.
type H23Balance struct {
	Account    auth.AccountName
	Balance    int64 // HIVE satoshi
	HBDBalance int64 // HBD satoshi
}

func h23BalanceKey(account auth.AccountName) []byte {
	return state.AppendAccountName(nil, account)
}

// AppendH23Balance serializes h.
func AppendH23Balance(dst []byte, h *H23Balance) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(h.Account)))
	dst = append(dst, h.Account...)
	dst = appendI64LE(dst, h.Balance)
	dst = appendI64LE(dst, h.HBDBalance)
	return dst
}

// ReadH23Balance decodes h.
func ReadH23Balance(src []byte) (H23Balance, error) {
	r := fcraw.NewReader(src)
	var h H23Balance
	name, err := r.ReadString()
	if err != nil {
		return h, fmt.Errorf("h23_balance.account: %w", err)
	}
	h.Account = name
	if h.Balance, err = r.ReadInt64(); err != nil {
		return h, fmt.Errorf("h23_balance.balance: %w", err)
	}
	if h.HBDBalance, err = r.ReadInt64(); err != nil {
		return h, fmt.Errorf("h23_balance.hbd_balance: %w", err)
	}
	if !r.AtEnd() {
		return h, errors.New("h23_balance: trailing bytes")
	}
	return h, nil
}

// PutH23Balance stores h. Note that hived's hardfork.d/0_23.hf code path
// inserts a row only on the first call for a given account (emplace with
// no replace); we mirror by overwriting unconditionally because the
// gather_balance call site is invoked exactly once per cleared account.
func PutH23Balance(s *state.Session, h *H23Balance) {
	s.Put(H23BalanceTable, state.PrimaryIndex, h23BalanceKey(h.Account), AppendH23Balance(nil, h))
}

// GetH23Balance fetches the row, or ErrNotFound if absent.
func GetH23Balance(s *state.Session, account auth.AccountName) (H23Balance, error) {
	v, err := s.Get(H23BalanceTable, state.PrimaryIndex, h23BalanceKey(account))
	if err != nil {
		return H23Balance{}, err
	}
	return ReadH23Balance(v)
}

// HasH23Balance reports existence.
func HasH23Balance(s *state.Session, account auth.AccountName) (bool, error) {
	return s.Has(H23BalanceTable, state.PrimaryIndex, h23BalanceKey(account))
}

// DeleteH23Balance removes the row.
func DeleteH23Balance(s *state.Session, account auth.AccountName) {
	s.Delete(H23BalanceTable, state.PrimaryIndex, h23BalanceKey(account))
}

// CountH23Balances scans the table and returns the number of rows. Used
// by parity tests that assert h23_balances.size().
func CountH23Balances(s *state.Session) (int, error) {
	n := 0
	err := s.Range(H23BalanceTable, state.PrimaryIndex, nil, func(_, _ []byte) (bool, error) {
		n++
		return true, nil
	})
	return n, err
}

// RangeH23Balances calls fn for every row in primary-key (account-name) order.
func RangeH23Balances(s *state.Session, fn func(h H23Balance) bool) error {
	return s.Range(H23BalanceTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
		h, err := ReadH23Balance(value)
		if err != nil {
			return false, err
		}
		return fn(h), nil
	})
}
