package chain

import (
	"math"
	"sort"
	"sync/atomic"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
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
	// C++ database.cpp:3628 — runs every block once HF25 is active. hived's
	// by_governance_vote_expiration_ts index short-circuits when the earliest
	// row is still in the future; we mirror that with the AccountStore's
	// govExpLB lower bound: O(1)-skip when nothing can be due (it DID
	// dominate: sorting+scanning ~1.6M accounts every block was ~5s/block at
	// 55M — the post-HF25 frontier crawl). When something IS due, one
	// UNORDERED scan collects the due names (sorted below — processing order
	// unchanged) and the exact next-due bound for the following blocks; the
	// noteDelayedVotes chokepoint keeps the bound valid between scans.
	accts := objects.EnsureAccountStore(s)
	type expired struct {
		name string
	}
	var due []expired
	if accts.GovExpHeapReady() {
		// Heap mode: O(log n) pop of genuinely-due accounts. Past the
		// HF25+365d onset expiries fire every ~40 blocks; the full-scan
		// fallback ate 77% of replay CPU there (run79: 172 blocks/s).
		for _, name := range accts.GovExpPopDue(blockTime) {
			due = append(due, expired{name: name})
		}
		accts.SetGovExpBound(accts.GovExpHeapTop())
		if len(due) == 0 {
			return
		}
	} else {
		if accts.GovExpNoneDue(blockTime) {
			return
		}
		// One-time seeding scan (fresh store, snapshot load, or rollback
		// reset): collect due accounts AND seed the heap with pending ones.
		nextDue := uint32(math.MaxUint32)
		accts.GovExpSeedStart(1 << 17)
		_ = accts.RangePrimaryUnordered(func(a *objects.Account) (bool, error) {
			e := a.GovernanceVoteExpiration
			if e == 0 {
				return true, nil
			}
			if e > blockTime {
				if e < nextDue {
					nextDue = e
				}
				accts.GovExpSeedAdd(a.Name, e)
				return true, nil
			}
			due = append(due, expired{name: a.Name})
			return true, nil
		})
		accts.GovExpSeedDone()
		// Install BEFORE processing: expirations written while clearing
		// (all 0 or future renewals) flow through noteDelayedVotes / the
		// heap push automatically.
		accts.SetGovExpBound(nextDue)
	}
	sort.Slice(due, func(i, j int) bool { return due[i].name < due[j].name })
	if envGhivediffGovExp != "" {
		for _, e := range due {
			ghiveDiff("GHIVEDIFF GOVEXP block=%d account=%s\n", blockNum, e.name)
		}
	}

	removed := 0
	defer func() { noteGovernanceRemovals(removed) }()
	for _, e := range due {
		a, err := objects.GetAccount(s, e.name)
		if err != nil {
			continue
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
		removed += clearProposalVotesForVoter(s, e.name)
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
// voter and removes them, returning how many it removed. The proposal_vote
// table is keyed (proposalID, voter); we scan the entire table since there's
// no by-voter secondary index.
//
// The count is what the governance sweeps meter against
// HIVE_GLOBAL_REMOVE_THRESHOLD — see governanceRemovalBudgetExceeded.
func clearProposalVotesForVoter(s *state.Session, voter string) int {
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
	return len(toDelete)
}

// Governance-sweep removal budget, NOT yet enforced.
//
// hived runs remove_expired_governance_votes and process_decline_voting_rights
// under one remove_guard( HIVE_GLOBAL_REMOVE_THRESHOLD ) shared across every
// account the block touches, counting proposal_vote removals. When an
// account's votes exhaust it, hived processes NOTHING further for that account
// -- no witness-vote clearing, no proxy clearing, no expiration bump -- and
// BREAKS, deferring that account and all later ones to the next block
// (database.cpp:3522). gopherd clears every due account in one block.
//
// That difference is consensus-visible: clearing an account's witness votes a
// block earlier than hived moves witness vote totals, which moves the
// schedule. It has evidently never bitten on mainnet through the live head --
// we would be rejecting blocks -- because the budget only counts PROPOSAL
// votes, and the accounts expiring in any one block rarely hold 200 of them
// between them.
//
// It is not ported yet because it cannot be ported correctly: hived iterates
// by_governance_vote_expiration_ts, whose tie-break is account_id, and gopherd
// has no account ids (the chainbase account-id refactor is still a plan). With
// name ordering the budget would defer a DIFFERENT set of accounts than hived
// does, which is worse than deferring none -- today's divergence needs the
// budget to be reached at all, that one would compound every block.
//
// So: measure first. These counters say whether the budget is ever reached,
// which is the evidence needed before the ordering work can be justified.
var (
	govSweepOverBudget  atomic.Uint64
	govSweepMaxPerBlock atomic.Uint64
)

// noteGovernanceRemovals records one block's proposal-vote removals from the
// governance sweeps and reports whether they exceeded what hived would allow
// in a single block.
func noteGovernanceRemovals(removed int) bool {
	if removed <= 0 {
		return false
	}
	for {
		cur := govSweepMaxPerBlock.Load()
		if uint64(removed) <= cur || govSweepMaxPerBlock.CompareAndSwap(cur, uint64(removed)) {
			break
		}
	}
	if removed > int(network.GlobalRemoveThreshold) {
		govSweepOverBudget.Add(1)
		return true
	}
	return false
}

// GovernanceRemovalStats reports how many blocks removed more proposal votes
// in the governance sweeps than hived's remove_guard would have allowed in one
// block, and the largest single-block count seen.
//
// A non-zero first value means gopherd and hived genuinely disagree about
// which block an account's governance votes were cleared in, and the ordering
// work above has to happen. Zero across a full replay means the budget is
// unreachable in practice on this chain.
func GovernanceRemovalStats() (blocksOverBudget, maxPerBlock uint64) {
	return govSweepOverBudget.Load(), govSweepMaxPerBlock.Load()
}
