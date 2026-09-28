package chain

import (
	"math"
	"sort"
	"sync/atomic"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
	"gopherd/internal/protocol/op"
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
		// expiration and created reproduce hived's by_governance_vote_expiration_ts
		// ordering — see the sort below.
		expiration uint32
		created    uint32
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
	// hived iterates by_governance_vote_expiration_ts, whose composite key is
	// (expiration_ts, account_id). Sorting by name alone was fine while every
	// due account was processed anyway; with a budget the ORDER decides which
	// accounts get deferred, so it has to match.
	//
	// gopherd has no account_id. Account.Created stands in for it — hived
	// assigns ids in creation order, so ordering by creation TIME agrees with
	// ordering by id for any two accounts created in different blocks. The
	// residue is accounts created within the same block, where this falls back
	// to name and hived would use the id. That is the same proxy already
	// validated for the liquidity-reward tie-break (7838fd8), and it is the
	// best available until accounts carry a creation sequence.
	for i := range due {
		if a, err := objects.GetAccount(s, due[i].name); err == nil {
			due[i].expiration = a.GovernanceVoteExpiration
			due[i].created = a.Created
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].expiration != due[j].expiration {
			return due[i].expiration < due[j].expiration
		}
		if due[i].created != due[j].created {
			return due[i].created < due[j].created
		}
		return due[i].name < due[j].name
	})
	if envGhivediffGovExp != "" {
		for _, e := range due {
			ghiveDiff("GHIVEDIFF GOVEXP block=%d account=%s\n", blockNum, e.name)
		}
	}

	removed := 0
	defer func() { noteGovernanceRemovals(removed) }()
	// hived's remove_guard. When an account's proposal-vote removal exhausts
	// it, hived does NOTHING further for that account — no witness-vote
	// clearing, no proxy clearing, no expiration bump — and BREAKS, deferring
	// it and every later account to the next block (database.cpp:3522). Its
	// votes that WERE removed stay removed, so the next block resumes rather
	// than restarting.
	//
	// Until now gopherd cleared every due account in one go. That is
	// consensus-visible whenever the budget is reached: an account's witness
	// votes cleared a block early move vote totals, and vote totals move the
	// schedule. It has never been reached on mainnet — govsweep-scan over the
	// whole post-HF21 chain found three over-budget slots, all in mid-2027 —
	// so enabling this changes nothing today and is correct when it does.
	guard := objects.NewRemoveGuard(network.GlobalRemoveThreshold)
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
		// Proposal votes first, and ONLY proceed if they all came out. hived
		// gates the entire rest of the account's processing on this
		// (`if( dhf_helper::remove_proposal_votes( ... ) )`), so a partial
		// removal must leave the account otherwise untouched.
		n, complete := removeProposalVotesForVoterBudgeted(s, e.name, guard)
		removed += n
		if !complete {
			// Deferred: this account and every later one wait for the next
			// block, exactly as hived's `break` does.
			break
		}
		objects.NullifyProxiedWitnessVotes(s, &a, directPower)
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
			d.pushVOp(op.ProxyCleared{
				Account: auth.AccountName(e.name),
				Proxy:   auth.AccountName(oldProxy),
			})
		}
		if d != nil {
			// hived emits this for every account it clears, AFTER any
			// proxy_cleared for the same account (database.cpp:3520).
			d.pushVOp(op.ExpiredAccountNotification{Account: auth.AccountName(e.name)})
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
// removeProposalVotesForVoterBudgeted is clearProposalVotesForVoter under a
// removal budget: it reports how many rows it erased and whether it got them
// all. Mirrors dhf_helper::remove_proposal_votes(voter, ...), whose bool
// return is what gates the rest of an account's governance clearing.
//
// Rows removed before the budget ran out STAY removed — hived does not roll
// them back — so the next block resumes where this one stopped rather than
// starting the account over.
func removeProposalVotesForVoterBudgeted(s *state.Session, voter string, g *objects.RemoveGuard) (removed int, complete bool) {
	for _, p := range proposalVotesOf(s, voter) {
		if !g.Step() {
			return removed, false
		}
		objects.DeleteProposalVote(s, p.proposalID, p.voter)
		removed++
	}
	return removed, true
}

func clearProposalVotesForVoter(s *state.Session, voter string) int {
	toDelete := proposalVotesOf(s, voter)
	for _, p := range toDelete {
		objects.DeleteProposalVote(s, p.proposalID, p.voter)
	}
	return len(toDelete)
}

type proposalVoteRef struct {
	proposalID int64
	voter      string
}

// proposalVotesOf collects every proposal_vote row cast BY one account. The
// table is keyed (proposalID, voter) with no by-voter index, so this scans;
// callers are the once-a-block governance paths, not per-operation code.
func proposalVotesOf(s *state.Session, voter string) []proposalVoteRef {
	var toDelete []proposalVoteRef
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
				toDelete = append(toDelete, proposalVoteRef{id, voter})
			}
			return true, nil
		})
	return toDelete
}

// Governance-sweep removal budget, NOT yet enforced.
//
// remove_expired_governance_votes and process_decline_voting_rights each open
// their OWN remove_guard( HIVE_GLOBAL_REMOVE_THRESHOLD ) — two independent
// budgets per block, not one shared between them (database.cpp:3501 and
// database_dhf.cpp:218) — and within a function the budget is shared across
// every account it touches, counting proposal_vote removals. When an account's
// votes exhaust it, hived processes NOTHING further for that account -- no
// witness-vote clearing, no proxy clearing, no expiration bump -- and BREAKS,
// deferring that account and all later ones to the next block
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

// noteGovernanceRemovals records ONE sweep's proposal-vote removals and reports
// whether they exceeded that sweep's budget. Called separately by each sweep,
// because hived gives each its own guard; summing them would invent a shared
// budget that does not exist and report divergence where there is none.
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

// GovernanceRemovalStats reports how many SWEEPS removed more proposal votes
// than hived's remove_guard would have allowed them, and the largest count a
// single sweep reached.
//
// A non-zero first value means gopherd and hived genuinely disagree about
// which block an account's governance votes were cleared in, and the ordering
// work above has to happen. Zero across a full replay means the budget is
// unreachable in practice on this chain.
func GovernanceRemovalStats() (blocksOverBudget, maxPerBlock uint64) {
	return govSweepOverBudget.Load(), govSweepMaxPerBlock.Load()
}
