package chain

import (
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// processDelayedVoting matures any delayed-vote buckets whose age has
// reached HIVE_DELAYED_VOTING_TOTAL_INTERVAL_SECONDS (30 days). For
// every matured bucket:
//
//   - the bucket is removed from the account's queue (erase_front)
//   - sum_delayed_votes -= bucket.val
//   - witness vote totals are incremented by bucket.val (the votes
//     are now "active")
//   - a delayed_voting virtual op would normally be emitted; we omit
//     it (consistent with our other vop suppressions)
//
// hived's index-by-oldest-bucket lets it process every due bucket in
// O(matured) per block; we scan all accounts. The cost is acceptable
// for now (mainnet has ~1.6M accounts; a full scan completes in
// hundreds of ms) and can be replaced with a secondary index later.
//
// Pre-HF24 this is a no-op — DelayedVotes is empty for every account.
func processDelayedVoting(s *state.Session, blockTime, blockNum uint32, d *Database) {
	hf := hardfork.SetForBlock(blockNum)
	if !hf.Has(hardfork.HF24) {
		return
	}

	// Fast path: skip the O(candidates) scan entirely on blocks where no
	// account's oldest delayed-vote bucket has matured yet (the common case —
	// maturations are spread across the 30-day window). Exact: never skips a
	// due account (see AccountStore.minFrontTime).
	if objects.DelayedVoteNoneDue(s, blockTime, objects.DelayedVotingTotalIntervalSec) {
		return
	}

	type matured struct {
		name string
		val  uint64
	}
	var due []matured

	// Iterate ONLY accounts with pending delayed votes (the index), in sorted
	// order — identical accounts + order to the old full RangeAccounts scan, but
	// without sorting+touching all ~1.4M accounts every block (which collapsed
	// replay from ~8000 to ~2 blk/s once HF24 activated ~block 47.6M). The index
	// is a conservative superset; we drop entries here once their queue empties.
	// Only the accounts whose front bucket has matured (hived walks
	// by_delayed_voting and stops at the first unexpired one).
	for _, name := range objects.DelayedVoteCandidatesDue(s, blockTime, objects.DelayedVotingTotalIntervalSec) {
		a, err := objects.GetAccount(s, name)
		if err != nil {
			objects.DropDelayedVoteCandidate(s, name)
			continue
		}
		if len(a.DelayedVotes) == 0 {
			objects.DropDelayedVoteCandidate(s, name) // stale index entry
			continue
		}
		acc := a // working copy; mutate via Put for a clean undo before-image
		changed := false
		for len(acc.DelayedVotes) > 0 {
			front := acc.DelayedVotes[0]
			if front.Time+objects.DelayedVotingTotalIntervalSec > blockTime {
				break
			}
			due = append(due, matured{name: acc.Name, val: front.Val})
			if err := objects.DelayedVotingEraseFront(&acc.DelayedVotes, &acc.SumDelayedVotes); err != nil {
				break
			}
			changed = true
		}
		if changed {
			objects.PutAccount(s, &acc)
		}
		if len(acc.DelayedVotes) == 0 {
			objects.DropDelayedVoteCandidate(s, name) // fully matured
		}
	}

	for _, m := range due {
		// Now that the bucket is mature, propagate the val to the
		// account's witness vote totals (and any proxy chain).
		// No val>0 guard — C++'s delayed_voting::run calls
		// adjust_proxied_witness_votes for every matured bucket.
		a, err := objects.GetAccount(s, m.name)
		if err != nil {
			continue
		}
		// util/delayed_voting.cpp:97 — pushed before adjust_proxied_witness_votes.
		if d != nil {
			d.pushVOp(op.DelayedVoting{Voter: auth.AccountName(m.name), Votes: m.val})
		}
		objects.AdjustProxiedWitnessVotes(s, &a, int64(m.val))
	}

	// Refresh the min-front-time lower bound so subsequent blocks can O(1)-skip
	// until the next-oldest bucket matures.
	objects.RecomputeDelayedVoteMin(s)
}
