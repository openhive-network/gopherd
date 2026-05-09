package chain

import (
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// clearExpiredDelegations mirrors database::clear_expired_delegations
// (libraries/chain/database.cpp:2326). For each
// vesting_delegation_expiration row whose `expiration` has passed,
// return the locked VESTS to the delegator's available pool by
// decrementing their `delegated_vesting_shares`.
func clearExpiredDelegations(s *state.Session, blockTime uint32) {
	if hint, ok := s.GetExpHint(objects.VestingDelegationExpirationTable, objects.VestingDelegationExpirationByDateIndex); ok && hint > blockTime {
		return
	}

	type pending struct {
		expiration uint32
		delegator  string
		id         uint32
	}
	var ready []pending
	var nextExp uint32

	_ = s.Range(objects.VestingDelegationExpirationTable, objects.VestingDelegationExpirationByDateIndex, nil,
		func(body, _ []byte) (bool, error) {
			if len(body) < 4+state.AccountNameKeyLen+4 {
				return true, nil
			}
			t := uint32(body[0])<<24 | uint32(body[1])<<16 | uint32(body[2])<<8 | uint32(body[3])
			// C++ database.cpp:3240 uses `expiration_time() < now` — strictly
			// less. Stop at `t >= blockTime` so equal-to-now rows wait one
			// more block, matching C++. (Before this change Go processed
			// `t <= blockTime`, expiring rows one block early — visible as
			// an off-by-one in delegator delegated_vesting_shares between
			// Go and C++ at the equality block.)
			if t >= blockTime {
				nextExp = t
				return false, nil
			}
			delegBytes := body[4 : 4+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && delegBytes[n-1] == 0 {
				n--
			}
			off := 4 + state.AccountNameKeyLen
			id := uint32(body[off])<<24 | uint32(body[off+1])<<16 | uint32(body[off+2])<<8 | uint32(body[off+3])
			ready = append(ready, pending{expiration: t, delegator: string(delegBytes[:n]), id: id})
			return true, nil
		})

	for _, p := range ready {
		v, err := s.Get(objects.VestingDelegationExpirationTable, state.PrimaryIndex,
			append(state.AppendAccountName(nil, p.delegator), state.AppendUint32BE(nil, p.id)...))
		if err != nil {
			continue
		}
		exp, err := objects.ReadVestingDelegationExpiration(v)
		if err != nil {
			continue
		}
		if a, gerr := objects.GetAccount(s, exp.Delegator); gerr == nil {
			a.DelegatedVestingShares -= exp.VestingShares
			if a.DelegatedVestingShares < 0 {
				a.DelegatedVestingShares = 0
			}
			objects.PutAccount(s, &a)
		}
		objects.DeleteVestingDelegationExpirationFull(s, &exp)
	}
	if nextExp == 0 {
		nextExp = ^uint32(0)
	}
	s.SetExpHint(objects.VestingDelegationExpirationTable, objects.VestingDelegationExpirationByDateIndex, nextExp)
}
