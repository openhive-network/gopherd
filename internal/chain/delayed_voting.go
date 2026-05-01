package chain

import (
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
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
func processDelayedVoting(s *state.Session, blockTime, blockNum uint32) {
	hf := hardfork.SetForBlock(blockNum)
	if !hf.Has(hardfork.HF24) {
		return
	}

	type matured struct {
		name string
		val  uint64
	}
	var due []matured

	_ = s.Range(objects.AccountTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			a, err := objects.ReadAccount(value)
			if err != nil {
				return true, nil
			}
			if len(a.DelayedVotes) == 0 {
				return true, nil
			}
			// Mature every bucket whose time + 30 days <= blockTime.
			// Buckets are ordered by time so the front is always the
			// oldest.
			changed := false
			for len(a.DelayedVotes) > 0 {
				front := a.DelayedVotes[0]
				if front.Time+objects.DelayedVotingTotalIntervalSec > blockTime {
					break
				}
				due = append(due, matured{name: a.Name, val: front.Val})
				if err := objects.DelayedVotingEraseFront(&a.DelayedVotes, &a.SumDelayedVotes); err != nil {
					break
				}
				changed = true
			}
			if changed {
				objects.PutAccount(s, &a)
			}
			return true, nil
		})

	for _, m := range due {
		// Now that the bucket is mature, propagate the val to the
		// account's witness vote totals (and any proxy chain).
		if m.val > 0 {
			a, err := objects.GetAccount(s, m.name)
			if err != nil {
				continue
			}
			objects.AdjustProxiedWitnessVotes(s, &a, int64(m.val))
		}
	}
}
