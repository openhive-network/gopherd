package objects

import (
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

// RemoveGuard mirrors hive::chain::remove_guard
// (libraries/chain/include/hive/chain/util/remove_guard.hpp).
//
// Erasing state objects costs the same whatever their type, so hived bounds a
// removal pass by a COUNT shared across every object it touches rather than
// per index. When the budget runs out mid-pass the caller is told, and it is
// the caller's job to leave the chain in a state where the rest can be
// finished later — for proposals, by flagging them.
//
// A negative limit means "no limit"; zero means the budget is spent.
type RemoveGuard struct {
	limit int16
}

// NewRemoveGuard returns a guard with the given budget. Pass a negative limit
// for an unbounded pass.
func NewRemoveGuard(limit int16) *RemoveGuard {
	return &RemoveGuard{limit: limit}
}

// Done reports whether the budget is exhausted.
func (g *RemoveGuard) Done() bool { return g.limit == 0 }

// Step consumes one unit of budget, reporting whether there was any. Matches
// remove_guard::step: a negative (unlimited) budget always succeeds and never
// decrements. Exported because the governance sweeps drive their own removal
// loops rather than going through the proposal helpers.
func (g *RemoveGuard) Step() bool { return g.step() }

func (g *RemoveGuard) step() bool {
	if g.limit > 0 {
		g.limit--
		return true
	}
	return !g.Done()
}

// RemoveProposalVotesForProposal erases the proposal_vote rows attached to
// `id`, in (proposal_id, voter) order — hived's by_proposal_voter — stopping
// as soon as the guard is spent. It reports whether every row was erased.
//
// Mirrors dhf_helper::remove_proposal_votes(proposal, ...).
func RemoveProposalVotesForProposal(s *state.Session, id int64, g *RemoveGuard) bool {
	prefix := state.AppendInt64BE(nil, id)
	// Collect first: Range shares one pooled iterator per session, so
	// mutating inside the callback is not safe.
	var voters []auth.AccountName
	_ = s.Range(ProposalVoteTable, state.PrimaryIndex, prefix,
		func(body, _ []byte) (bool, error) {
			if len(body) < 8+state.AccountNameKeyLen {
				return true, nil
			}
			voters = append(voters, decodeAccountNameKey(body[8:8+state.AccountNameKeyLen]))
			return true, nil
		})
	for _, v := range voters {
		if !g.step() {
			return false
		}
		DeleteProposalVote(s, id, v)
	}
	return true
}

// RemoveProposalWithVotes erases a proposal together with its votes, as far as
// the guard allows, and reports whether the PROPOSAL itself was erased.
//
// Mirrors dhf_helper::remove_proposal: the vote sweep's own result is ignored,
// so a pass that runs dry part-way through the votes leaves both the remaining
// votes and the proposal in place, and the return value is false.
func RemoveProposalWithVotes(s *state.Session, p *Proposal, g *RemoveGuard) bool {
	RemoveProposalVotesForProposal(s, p.ID, g)
	if !g.step() {
		return false
	}
	DeleteProposalFull(s, p)
	return true
}

// decodeAccountNameKey reverses state.AppendAccountName: a fixed-width,
// NUL-padded name.
func decodeAccountNameKey(b []byte) auth.AccountName {
	n := len(b)
	for n > 0 && b[n-1] == 0 {
		n--
	}
	return auth.AccountName(string(b[:n]))
}
