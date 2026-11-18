package evaluator

import (
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// RC usage of delegate_rc_operation, shared by its two forms. The regular
// operation (tag 37, HF29) is charged like every other regular operation:
// the chain package takes the state discount before the evaluator runs and
// the cost when it counts the transaction. The form embedded in an "rc"
// custom_json is charged by its evaluator, per operation that actually runs
// (see applyRCCustomJSON), because only the interpreter knows the payload.

const (
	// DelegateRCBaseStateSize is state_object_size_info::delegate_rc_base_size
	// (resource_sizes.cpp): SIZE(rc_direct_delegation_index node) = 88 ×
	// LASTING_STATE_BYTE (1 × 5 × 365 × 12), per delegatee.
	DelegateRCBaseStateSize int64 = 88 * 5 * 365 * 12
	// DelegateRCExecTimeNS is operation_exec_info::delegate_rc_time.
	DelegateRCExecTimeNS int64 = 75000
)

// DelegateRCUsage is what count_operation_visitor adds for one
// delegate_rc_operation (resource_count.cpp): state for each delegatee unless
// the delegations are being removed, and its execution time.
func DelegateRCUsage(maxRC int64, delegatees int) (stateBytes, execTime int64) {
	if maxRC != 0 {
		stateBytes = DelegateRCBaseStateSize * int64(delegatees)
	}
	return stateBytes, DelegateRCExecTimeNS
}

// DelegateRCStateDiscount is count_differential_operation_visitor for
// delegate_rc_operation (resource_count.cpp): the state of the delegations the
// operation is about to overwrite, evaluated against the state as it is right
// before the operation. Nothing when the delegations are being removed, or when
// `from` or any delegatee does not exist. delegatees is the flat_set.
func DelegateRCStateDiscount(s *state.Session, from string, delegatees []string, maxRC int64) int64 {
	if maxRC == 0 {
		return 0
	}
	if has, _ := objects.HasAccount(s, auth.AccountName(from)); !has {
		return 0
	}
	var usage int64
	for _, to := range delegatees {
		if has, _ := objects.HasAccount(s, auth.AccountName(to)); !has {
			return 0
		}
		if _, err := objects.GetRCDirectDelegation(s, auth.AccountName(from), auth.AccountName(to)); err == nil {
			usage += DelegateRCBaseStateSize
		}
	}
	return usage
}
