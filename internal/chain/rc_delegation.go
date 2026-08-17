package chain

import (
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// ApplyRCDelegation applies a (from → to, max_rc) RC delegation. Mirrors
// hived's rc_custom_operation handler for delegate_rc:
//
//   - If max_rc == existing → no-op (asserted in hived)
//   - Updates from.DelegatedRCOut and to.DelegatedRCIn by the delta
//   - Stores or updates the rc_direct_delegation_object row
//   - max_rc == 0 means remove the delegation
//
// Both sides' RC manabars need a refresh-after-vest-change to pick up
// the new max_rc; callers should invoke RegenerateRCMana on both
// accounts after this returns.
func ApplyRCDelegation(s *state.Session, from, to auth.AccountName, maxRC int64) error {
	if from == to || maxRC < 0 {
		return nil
	}
	prev, err := objects.GetRCDirectDelegation(s, from, to)
	hadPrev := err == nil
	prevAmt := int64(0)
	if hadPrev {
		prevAmt = int64(prev.DelegatedRC)
	}
	delta := maxRC - prevAmt
	if delta == 0 {
		return nil
	}
	fromAcct, err := objects.GetAccount(s, from)
	if err != nil {
		return err
	}
	toAcct, err := objects.GetAccount(s, to)
	if err != nil {
		return err
	}
	fromAcct.DelegatedRCOut += delta
	toAcct.DelegatedRCIn += delta
	if fromAcct.DelegatedRCOut < 0 {
		fromAcct.DelegatedRCOut = 0
	}
	if toAcct.DelegatedRCIn < 0 {
		toAcct.DelegatedRCIn = 0
	}
	objects.PutAccount(s, &fromAcct)
	objects.PutAccount(s, &toAcct)

	if maxRC == 0 {
		objects.DeleteRCDirectDelegation(s, from, to)
	} else {
		objects.PutRCDirectDelegation(s, &objects.RCDirectDelegation{
			From: from, To: to, DelegatedRC: uint64(maxRC),
		})
	}
	return nil
}
