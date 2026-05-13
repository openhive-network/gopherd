package chain

import (
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
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
//   - clear any proxy
//   - mark the account VoteRevoked = true
//   - remove the request
func processDeclineVotingRights(s *state.Session, blockTime, blockNum uint32) {
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
	for _, p := range ready {
		req, err := objects.GetDeclineVotingRightsRequest(s, p.account)
		if err != nil {
			continue
		}
		a, aerr := objects.GetAccount(s, p.account)
		if aerr == nil {
			// HF28: drop any DHF proposal votes the account has cast.
			// Mirrors database_dhf.cpp:223 (remove_proposal_votes).
			if hf.Has(hardfork.HF28) {
				clearProposalVotesForVoter(s, p.account)
			}
			// nullify_proxied_witness_votes: subtract the account's full
			// propagation (direct + proxied_vsf_votes[*]) from each
			// witness it votes for or from its proxy chain. Mirrors
			// database.cpp:1131.
			directVotePower := a.VestingShares
			objects.NullifyProxiedWitnessVotes(s, &a, directVotePower)
			// clear_witness_votes: remove vote rows. Pass voterVesting=0
			// so the rows are removed without further subtraction (the
			// nullify call above already adjusted witness totals).
			objects.ClearWitnessVotesForAccount(s, p.account, 0)
			a.WitnessesVotedFor = 0
			a.Proxy = ""
			a.VoteRevoked = true
			objects.PutAccount(s, &a)
		}
		objects.DeleteDeclineVotingRightsRequestFull(s, &req)
	}
	if nextExp == 0 {
		nextExp = ^uint32(0)
	}
	s.SetExpHint(objects.DeclineVotingRightsRequestTable, objects.DeclineVotingRightsByEffectiveIndex, nextExp)
}
