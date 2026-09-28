package chain

import (
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// processDeclineVotingRights mirrors database::process_decline_voting_rights
// (libraries/chain/database_dhf.cpp:207). For every
// decline_voting_rights_request whose effective_date has passed:
//   - HF28+: clear any DHF proposal_votes the account has cast
//   - nullify_proxied_witness_votes: subtract the account's full proxy
//     propagation (direct vest + proxied_vsf_votes[*]) from every
//     witness it votes for (or from its proxy chain's terminal voter)
//   - clear_witness_votes: remove the witness_vote rows and zero
//     witnesses_voted_for
//   - clear any proxy (emit proxy_cleared_operation virtual op if the
//     account had a non-empty proxy when this fires)
//   - mark the account VoteRevoked = true
//   - remove the request
//
// The `d` parameter is the chain.Database — passed in so we can push
// proxy_cleared virtual ops onto the per-block buffer (block_vops.go).
// May be nil in test contexts that bypass the vop buffer.
func processDeclineVotingRights(s *state.Session, blockTime, blockNum uint32, d *Database) {
	if hint, ok := s.GetExpHint(objects.DeclineVotingRightsRequestTable, objects.DeclineVotingRightsByEffectiveIndex); ok && hint > blockTime {
		return
	}

	type pending struct {
		effective uint32
		account   string
	}
	var ready []pending
	var nextExp uint32

	_ = s.Range(objects.DeclineVotingRightsRequestTable, objects.DeclineVotingRightsByEffectiveIndex, nil,
		func(body, _ []byte) (bool, error) {
			if len(body) < 4+state.AccountNameKeyLen {
				return true, nil
			}
			t := uint32(body[0])<<24 | uint32(body[1])<<16 | uint32(body[2])<<8 | uint32(body[3])
			if t > blockTime {
				nextExp = t
				return false, nil
			}
			nameBytes := body[4 : 4+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && nameBytes[n-1] == 0 {
				n--
			}
			ready = append(ready, pending{effective: t, account: string(nameBytes[:n])})
			return true, nil
		})

	hf := hardfork.SetForBlock(blockNum)
	// hived opens a remove_guard( HIVE_GLOBAL_REMOVE_THRESHOLD ) here
	// (database_dhf.cpp:218), SEPARATE from the one the governance-expiry
	// sweep opens — each gets a fresh budget — and gates the rest of an
	// account's processing on the removal completing:
	//
	//	if( !has_hardfork( HF28 ) || dhf_helper::remove_proposal_votes( ... ) )
	//
	// Note the short-circuit: before HF28 the removal is not attempted at all
	// and the account is always processed, which is why the budget only binds
	// from HF28 onward.
	removed := 0
	defer func() { noteGovernanceRemovals(removed) }()
	guard := objects.NewRemoveGuard(network.GlobalRemoveThreshold)
	for _, p := range ready {
		req, err := objects.GetDeclineVotingRightsRequest(s, p.account)
		if err != nil {
			continue
		}
		a, aerr := objects.GetAccount(s, p.account)
		if aerr == nil {
			// HF28: drop any DHF proposal votes the account has cast.
			// Mirrors database_dhf.cpp:224. When the budget runs out part-way
			// hived logs "Threshold exceeded while deleting proposal votes"
			// and BREAKS — the account keeps its request row and is retried
			// next block, along with everything after it. Same as the
			// governance-expiry sweep; I assumed it continued and the source
			// says otherwise.
			if hf.Has(hardfork.HF28) {
				n, complete := removeProposalVotesForVoterBudgeted(s, p.account, guard)
				removed += n
				if !complete {
					break
				}
			}
			// nullify_proxied_witness_votes: subtract the account's full
			// propagation (direct + proxied_vsf_votes[*]) from each
			// witness it votes for or from its proxy chain. Mirrors
			// database.cpp:1131. directVotePower must match C++
			// account_object::get_direct_governance_vote_power(): pre-HF24
			// just vesting_shares; HF24+ subtracts sum_delayed_votes so
			// delayed VESTS don't double-count when removed.
			directVotePower := a.VestingShares
			if hf.Has(hardfork.HF24) {
				directVotePower -= int64(a.SumDelayedVotes)
				if directVotePower < 0 {
					directVotePower = 0
				}
			}
			objects.NullifyProxiedWitnessVotes(s, &a, directVotePower)
			// clear_witness_votes: remove vote rows. Pass voterVesting=0
			// so the rows are removed without further subtraction (the
			// nullify call above already adjusted witness totals).
			objects.ClearWitnessVotesForAccount(s, p.account, 0)
			a.WitnessesVotedFor = 0
			oldProxy := a.Proxy
			a.Proxy = ""
			a.VoteRevoked = true
			objects.PutAccount(s, &a)
			// hived emits proxy_cleared_operation when the settled
			// decline_voting_rights request clears a non-empty proxy.
			// Mirrors libraries/chain/database_dhf.cpp:225 (push_virtual_operation
			// of proxy_cleared_operation in process_decline_voting_rights).
			if d != nil && oldProxy != "" {
				d.pushVOp(BlockVOp{
					Kind:    BlockVOpProxyCleared,
					Account: auth.AccountName(p.account),
					Proxy:   auth.AccountName(oldProxy),
				})
			}
		}
		objects.DeleteDeclineVotingRightsRequestFull(s, &req)
	}
	if nextExp == 0 {
		nextExp = ^uint32(0)
	}
	s.SetExpHint(objects.DeclineVotingRightsRequestTable, objects.DeclineVotingRightsByEffectiveIndex, nextExp)
}
