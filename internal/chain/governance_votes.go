package chain

import (
	"fmt"
	"os"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// removeExpiredGovernanceVotes mirrors database::remove_expired_governance_votes
// (libraries/chain/database.cpp:3630). HF25+ (NOT HF22 — the gate moved
// up at HF25 to coincide with the renewal-period activation): when an
// account hasn't updated its governance_vote_expiration_ts within the
// renewal period (1 year), its proposal votes, witness proxy, and
// witness votes are cleared. C++ first calls nullify_proxied_witness_votes
// to drain proxied weight up the chain BEFORE clear_witness_votes.
//
// Pre-HF25 the field is unused for sweeps; pre-HF22 the field is
// uninitialized and this hook is a no-op.
//
// We approximate by scanning the accounts table once per maintenance
// interval (every block has acceptable cost given Pebble's iteration);
// hived uses a by-expiration secondary index. For replay correctness
// the difference is only timing — both approaches converge on the
// same final state once all due accounts are processed.
// The `d` parameter is the chain.Database — passed in so we can push
// proxy_cleared virtual ops onto the per-block buffer (block_vops.go).
// May be nil in test contexts that bypass the vop buffer.
func removeExpiredGovernanceVotes(s *state.Session, blockTime, blockNum uint32, d *Database) {
	hf := hardfork.SetForBlock(blockNum)
	if !hf.Has(hardfork.HF25) {
		return
	}
	// C++ database.cpp:3628 — runs every block once HF25 is active. The
	// by_governance_vote_expiration_ts index lets hived short-circuit
	// when the earliest row is still in the future; we don't yet have
	// that secondary index, so we scan all accounts. The total cost is
	// bounded by Account.RangePrimary which is O(n_accounts) per block.
	// TODO(perf): build a by-expiration index if this dominates.

	type expired struct {
		name string
	}
	var due []expired
	_ = objects.RangeAccounts(s, func(a *objects.Account) (bool, error) {
		if a.GovernanceVoteExpiration == 0 {
			return true, nil
		}
		if a.GovernanceVoteExpiration > blockTime {
			return true, nil
		}
		due = append(due, expired{name: a.Name})
		return true, nil
	})

	if os.Getenv("GHIVEDBG_SWEEP") != "" && len(due) > 0 {
		fmt.Fprintf(os.Stderr, "SWEEP blockTime=%d due=%d\n", blockTime, len(due))
		for _, e := range due {
			fmt.Fprintf(os.Stderr, "  due: %s\n", e.name)
		}
	}
	for _, e := range due {
		a, err := objects.GetAccount(s, e.name)
		if err != nil {
			continue
		}
		if os.Getenv("GHIVEDBG_SWEEP") != "" {
			fmt.Fprintf(os.Stderr, "  processing %s\n", e.name)
		}
		// C++ database.cpp:3633-3634 — nullify proxied votes BEFORE
		// clearing direct votes, so accumulated proxied_vsf_votes drain
		// up the chain rather than leaking on the cleared account.
		directPower := a.VestingShares
		if hf.Has(hardfork.HF24) {
			directPower -= int64(a.SumDelayedVotes)
			if directPower < 0 {
				directPower = 0
			}
		}
		objects.NullifyProxiedWitnessVotes(s, &a, directPower)
		// Drop all proposal votes by this account.
		if os.Getenv("GHIVEDBG_SWEEP") != "" {
			fmt.Fprintf(os.Stderr, "  clearProposalVotesForVoter(%s)\n", e.name)
		}
		clearProposalVotesForVoter(s, e.name)
		// Drop direct witness votes. Pass 0 because Nullify above
		// already removed the propagation-array contribution for the
		// chain-walk's witnesses.
		objects.ClearWitnessVotesForAccount(s, e.name, 0)
		// Re-read after possible mutations from NullifyProxiedWitnessVotes.
		if fresh, ferr := objects.GetAccount(s, e.name); ferr == nil {
			a = fresh
		}
		oldProxy := a.Proxy
		a.Proxy = ""
		a.WitnessesVotedFor = 0
		a.GovernanceVoteExpiration = 0
		objects.PutAccount(s, &a)
		// hived emits proxy_cleared_operation when the 365-day governance
		// vote expiration sweep clears a non-empty proxy. Mirrors
		// libraries/chain/database.cpp:3650 (push_virtual_operation of
		// proxy_cleared_operation in remove_expired_governance_votes).
		if d != nil && oldProxy != "" {
			d.pushVOp(BlockVOp{
				Kind:    BlockVOpProxyCleared,
				Account: auth.AccountName(e.name),
				Proxy:   auth.AccountName(oldProxy),
			})
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
	if os.Getenv("GHIVEDBG_SWEEP") != "" {
		fmt.Fprintf(os.Stderr, "    clearProposalVotesForVoter(%s) -> %d rows\n", voter, len(toDelete))
		for _, p := range toDelete {
			fmt.Fprintf(os.Stderr, "      delete (pid=%d, voter=%s)\n", p.proposalID, p.voter)
		}
	}
	for _, p := range toDelete {
		objects.DeleteProposalVote(s, p.proposalID, p.voter)
	}
}
