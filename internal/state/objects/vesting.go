package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// VestingDelegation is the vesting_delegation_object — outstanding VESTS
// delegated from delegator to delegatee.
type VestingDelegation struct {
	Delegator     auth.AccountName
	Delegatee     auth.AccountName
	VestingShares int64
	MinDelegation uint32 // time_point_sec when delegation can be reduced
}

// WithdrawRoute is the withdraw_vesting_route_object — diverts a percentage
// of an account's vesting withdrawal to another account, optionally as VESTS
// (auto_vest) instead of liquid HIVE.
type WithdrawRoute struct {
	From     auth.AccountName
	To       auth.AccountName
	Percent  uint16
	AutoVest bool
}

const (
	VestingDelegationTable state.TableID = 8
	WithdrawRouteTable     state.TableID = 9
	// VestingWithdrawalScheduleTable indexes accounts with active power-down
	// schedules by next-withdrawal time. Key: [next_withdrawal_BE_4][account_name_padded_16].
	// Value is the empty byte slice — the only data we need is the (time, account)
	// pair encoded in the key. process_vesting_withdrawals iterates this in
	// time order each block to find ready payouts.
	VestingWithdrawalScheduleTable state.TableID = 18
)

// VestingWithdrawalScheduleKey encodes (nextWithdrawal, account) for the
// schedule index.
func VestingWithdrawalScheduleKey(nextWithdrawal uint32, account auth.AccountName) []byte {
	out := state.AppendUint32BE(nil, nextWithdrawal)
	return state.AppendAccountName(out, account)
}

// PutVestingWithdrawalSchedule registers (nextWithdrawal, account) in the
// schedule index. Now routed through the typed store.
func PutVestingWithdrawalSchedule(s *state.Session, nextWithdrawal uint32, account auth.AccountName) {
	EnsureVestingWithdrawalScheduleStore(s).Put(nextWithdrawal, account)
}

// DeleteVestingWithdrawalSchedule removes (nextWithdrawal, account) from the
// schedule index.
func DeleteVestingWithdrawalSchedule(s *state.Session, nextWithdrawal uint32, account auth.AccountName) {
	EnsureVestingWithdrawalScheduleStore(s).Delete(nextWithdrawal, account)
}

func vestingDelegationKey(delegator, delegatee auth.AccountName) []byte {
	out := state.AppendAccountName(nil, delegator)
	return state.AppendAccountName(out, delegatee)
}

func AppendVestingDelegation(dst []byte, d *VestingDelegation) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(d.Delegator)))
	dst = append(dst, d.Delegator...)
	dst = fcraw.AppendVarUint(dst, uint32(len(d.Delegatee)))
	dst = append(dst, d.Delegatee...)
	dst = appendI64LE(dst, d.VestingShares)
	dst = appendU32LE(dst, d.MinDelegation)
	return dst
}

func ReadVestingDelegation(src []byte) (VestingDelegation, error) {
	r := fcraw.NewReader(src)
	var d VestingDelegation
	delegator, err := r.ReadString()
	if err != nil {
		return d, err
	}
	d.Delegator = delegator
	if d.Delegatee, err = r.ReadString(); err != nil {
		return d, err
	}
	if d.VestingShares, err = r.ReadInt64(); err != nil {
		return d, err
	}
	if d.MinDelegation, err = r.ReadUint32(); err != nil {
		return d, err
	}
	if !r.AtEnd() {
		return d, errors.New("vesting_delegation: trailing bytes")
	}
	return d, nil
}

func PutVestingDelegation(s *state.Session, d *VestingDelegation) {
	s.Put(VestingDelegationTable, state.PrimaryIndex, vestingDelegationKey(d.Delegator, d.Delegatee), AppendVestingDelegation(nil, d))
}

func GetVestingDelegation(s *state.Session, delegator, delegatee auth.AccountName) (VestingDelegation, error) {
	v, err := s.Get(VestingDelegationTable, state.PrimaryIndex, vestingDelegationKey(delegator, delegatee))
	if err != nil {
		return VestingDelegation{}, err
	}
	return ReadVestingDelegation(v)
}

func HasVestingDelegation(s *state.Session, delegator, delegatee auth.AccountName) (bool, error) {
	return s.Has(VestingDelegationTable, state.PrimaryIndex, vestingDelegationKey(delegator, delegatee))
}

func DeleteVestingDelegation(s *state.Session, delegator, delegatee auth.AccountName) {
	s.Delete(VestingDelegationTable, state.PrimaryIndex, vestingDelegationKey(delegator, delegatee))
}

func withdrawRouteKey(from, to auth.AccountName) []byte {
	out := state.AppendAccountName(nil, from)
	return state.AppendAccountName(out, to)
}

func AppendWithdrawRoute(dst []byte, r *WithdrawRoute) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(r.From)))
	dst = append(dst, r.From...)
	dst = fcraw.AppendVarUint(dst, uint32(len(r.To)))
	dst = append(dst, r.To...)
	dst = append(dst, byte(r.Percent), byte(r.Percent>>8))
	if r.AutoVest {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	return dst
}

func ReadWithdrawRoute(src []byte) (WithdrawRoute, error) {
	r := fcraw.NewReader(src)
	var wr WithdrawRoute
	from, err := r.ReadString()
	if err != nil {
		return wr, err
	}
	wr.From = from
	if wr.To, err = r.ReadString(); err != nil {
		return wr, err
	}
	if wr.Percent, err = r.ReadUint16(); err != nil {
		return wr, err
	}
	if wr.AutoVest, err = r.ReadBool(); err != nil {
		return wr, err
	}
	if !r.AtEnd() {
		return wr, fmt.Errorf("withdraw_route: trailing bytes")
	}
	return wr, nil
}

func PutWithdrawRoute(s *state.Session, r *WithdrawRoute) {
	s.Put(WithdrawRouteTable, state.PrimaryIndex, withdrawRouteKey(r.From, r.To), AppendWithdrawRoute(nil, r))
}

func DeleteWithdrawRoute(s *state.Session, from, to auth.AccountName) {
	s.Delete(WithdrawRouteTable, state.PrimaryIndex, withdrawRouteKey(from, to))
}

func GetWithdrawRoute(s *state.Session, from, to auth.AccountName) (WithdrawRoute, error) {
	v, err := s.Get(WithdrawRouteTable, state.PrimaryIndex, withdrawRouteKey(from, to))
	if err != nil {
		return WithdrawRoute{}, err
	}
	return ReadWithdrawRoute(v)
}

// SumWithdrawRoutePercentFrom returns the total percent across all
// withdraw_vesting_route rows whose from_account == from. Used by the
// evaluator to enforce the HIVE_100_PERCENT cap (hive_evaluator_transfer.cpp:453).
func SumWithdrawRoutePercentFrom(s *state.Session, from auth.AccountName) (uint32, error) {
	prefix := state.AppendAccountName(nil, from)
	var total uint32
	err := s.Range(WithdrawRouteTable, state.PrimaryIndex, prefix, func(_, value []byte) (bool, error) {
		wr, rerr := ReadWithdrawRoute(value)
		if rerr != nil {
			return false, rerr
		}
		if wr.From != from {
			return false, nil
		}
		total += uint32(wr.Percent)
		return true, nil
	})
	return total, err
}
