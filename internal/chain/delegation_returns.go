package chain

import (
	"fmt"
	"os"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// envDelegretTrace (GHIVEDIFF_DELEGRET=1) traces every delegation-return
// sweep: hint skips, ready counts, primary-row misses, per-return credits.
// Diagnostic for the 40M-checkpoint resume post-mortem (returns present in
// state but observably not processed).
var envDelegretTrace = os.Getenv("GHIVEDIFF_DELEGRET")

// clearExpiredDelegations mirrors database::clear_expired_delegations
// (libraries/chain/database.cpp:2326). For each
// vesting_delegation_expiration row whose `expiration` has passed,
// return the locked VESTS to the delegator's available pool by
// decrementing their `delegated_vesting_shares`.
func clearExpiredDelegations(s *state.Session, blockTime uint32) {
	if hint, ok := s.GetExpHint(objects.VestingDelegationExpirationTable, objects.VestingDelegationExpirationByDateIndex); ok && hint > blockTime {
		if envDelegretTrace != "" {
			fmt.Fprintf(os.Stderr, "DELEGRET skip blockTime=%d hint=%d\n", blockTime, hint)
		}
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

	var hf20 bool
	var dvPoolPct uint16
	var headBlockNum uint32
	if len(ready) > 0 {
		if dgpoNow, derr := objects.GetDGPO(s); derr == nil {
			headBlockNum = dgpoNow.HeadBlockNumber
			hf20 = hardfork.SetForBlock(headBlockNum).Has(hardfork.HF20)
			dvPoolPct = dgpoNow.DownvotePoolPercent
		}
	}
	if envDelegretTrace != "" && len(ready) > 0 {
		fmt.Fprintf(os.Stderr, "DELEGRET sweep blockTime=%d ready=%d nextExp=%d\n", blockTime, len(ready), nextExp)
	}
	for _, p := range ready {
		v, err := s.Get(objects.VestingDelegationExpirationTable, state.PrimaryIndex,
			append(state.AppendAccountName(nil, p.delegator), state.AppendUint32BE(nil, p.id)...))
		if err != nil {
			if envDelegretTrace != "" {
				fmt.Fprintf(os.Stderr, "DELEGRET MISS-PRIMARY delegator=%s id=%d exp=%d\n", p.delegator, p.id, p.expiration)
			}
			continue
		}
		exp, err := objects.ReadVestingDelegationExpiration(v)
		if err != nil {
			continue
		}
		if envDelegretTrace != "" {
			fmt.Fprintf(os.Stderr, "DELEGRET return delegator=%s vests=%d exp=%d\n", exp.Delegator, exp.VestingShares, exp.Expiration)
		}
		a, gerr := objects.GetAccount(s, exp.Delegator)
		if gerr != nil {
			// Skip the expiration record entirely if the delegator is
			// missing — deleting it without crediting back the
			// delegated_vesting_shares leaks the delegated amount and
			// blocks the delegator from ever delegating that VESTS chunk
			// again. The row will be retried on the next sweep.
			continue
		}
		// HF20+: returned vests re-enter the delegator's voting mana —
		// regen at the pre-return cap, bump by the returned amount
		// (database.cpp:3143-3151), before delegated_vesting shrinks.
		if hf20 {
			objects.RegenerateRCMana(&a, blockTime)
			objects.UpdateVotingManabar(&a, blockTime, dvPoolPct, exp.VestingShares, headBlockNum)
		}
		a.DelegatedVestingShares -= exp.VestingShares
		if a.DelegatedVestingShares < 0 {
			a.DelegatedVestingShares = 0
		}
		if hf20 {
			// The regenerate above is the BEFORE half, and hived's
			// update_account_after_vest_change is the AFTER half — without it
			// the account is written back carrying a last_max_rc computed from
			// the pre-return delegation, and the next reader anywhere finds it
			// stale. That made this the largest remaining source of stale-cache
			// hits once the post-change sites were reclassified. The
			// regeneration inside it is a no-op here (same block time); what
			// matters is the refreshed ceiling.
			objects.UpdateAccountAfterVestChange(&a, blockTime)
		}
		objects.PutAccount(s, &a)
		objects.DeleteVestingDelegationExpirationFull(s, &exp)
	}
	if nextExp == 0 {
		nextExp = ^uint32(0)
	}
	s.SetExpHint(objects.VestingDelegationExpirationTable, objects.VestingDelegationExpirationByDateIndex, nextExp)
}
