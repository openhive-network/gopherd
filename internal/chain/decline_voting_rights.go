package chain

import (
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// processDeclineVotingRights mirrors database::process_decline_voting_rights
// (libraries/chain/database_dhf.cpp:207). For every
// decline_voting_rights_request whose effective_date has passed:
//   - clear the account's witness votes (and decrement each affected
//     witness's total_votes_vests)
//   - clear any proxy
//   - mark the account VoteRevoked = true
//   - remove the request
func processDeclineVotingRights(s *state.Session, blockTime uint32) {
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

	for _, p := range ready {
		req, err := objects.GetDeclineVotingRightsRequest(s, p.account)
		if err != nil {
			continue
		}
		a, aerr := objects.GetAccount(s, p.account)
		if aerr == nil {
			objects.ClearWitnessVotesForAccount(s, p.account, a.VestingShares)
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
