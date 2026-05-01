package chain

import (
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// removeExpiredGovernanceVotes mirrors database::remove_expired_governance_votes
// (libraries/chain/database.cpp:3505). HF22+: when an account hasn't
// updated its governance_vote_expiration_ts within the renewal period
// (1 year), its proposal votes and witness votes are cleared.
//
// Pre-HF22 the field is unused and this hook is a no-op.
//
// We approximate by scanning the accounts table once per maintenance
// interval (every block has acceptable cost given Pebble's iteration);
// hived uses a by-expiration secondary index. For replay correctness
// the difference is only timing — both approaches converge on the
// same final state once all due accounts are processed.
func removeExpiredGovernanceVotes(s *state.Session, blockTime, blockNum uint32) {
	hf := hardfork.SetForBlock(blockNum)
	if !hf.Has(hardfork.HF22) {
		return
	}
	// Only run once per HIVE_PROPOSAL_MAINTENANCE_PERIOD (1h) to bound
	// the cost.
	if blockTime%proposalMaintenancePeriodSec != 0 {
		return
	}

	type expired struct {
		name    string
		vesting int64
	}
	var due []expired
	_ = s.Range(objects.AccountTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			a, err := objects.ReadAccount(value)
			if err != nil {
				return true, nil
			}
			if a.GovernanceVoteExpiration == 0 {
				return true, nil
			}
			if a.GovernanceVoteExpiration > blockTime {
				return true, nil
			}
			due = append(due, expired{name: a.Name, vesting: a.VestingShares})
			return true, nil
		})

	for _, e := range due {
		// Drop all proposal votes by this account.
		clearProposalVotesForVoter(s, e.name)
		// Drop all direct witness votes (and decrement totals).
		objects.ClearWitnessVotesForAccount(s, e.name, e.vesting)
		// Clear proxy + flag expired.
		if a, err := objects.GetAccount(s, e.name); err == nil {
			a.Proxy = ""
			a.WitnessesVotedFor = 0
			// Sentinel: setting to 0 means "expired/never expired again".
			a.GovernanceVoteExpiration = 0
			objects.PutAccount(s, &a)
		}
	}
}

// clearProposalVotesForVoter walks all proposal_vote rows by the named
// voter and removes them. The proposal_vote table is keyed
// (proposalID, voter); we scan the entire table since there's no
// by-voter secondary index.
func clearProposalVotesForVoter(s *state.Session, voter string) {
	type pair struct {
		proposalID int64
		voter      string
	}
	var toDelete []pair
	_ = s.Range(objects.ProposalVoteTable, state.PrimaryIndex, nil,
		func(body, _ []byte) (bool, error) {
			if len(body) < 8+state.AccountNameKeyLen {
				return true, nil
			}
			var u uint64
			for i := 0; i < 8; i++ {
				u = (u << 8) | uint64(body[i])
			}
			id := int64(u ^ (1 << 63))
			voterBytes := body[8 : 8+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && voterBytes[n-1] == 0 {
				n--
			}
			if string(voterBytes[:n]) == voter {
				toDelete = append(toDelete, pair{id, voter})
			}
			return true, nil
		})
	for _, p := range toDelete {
		objects.DeleteProposalVote(s, p.proposalID, p.voter)
	}
}
