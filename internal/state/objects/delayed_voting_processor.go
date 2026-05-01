package objects

import "errors"

// HIVE_DELAYED_VOTING_INTERVAL_SECONDS — bucket width (1 day). Within a
// single bucket, all newly-added delayed votes coalesce.
//
// Mirrors libraries/protocol/include/hive/protocol/config.hpp.
const DelayedVotingIntervalSec uint32 = 60 * 60 * 24

// HIVE_DELAYED_VOTING_TOTAL_INTERVAL_SECONDS — total maturation window
// (30 days). After this many seconds from a bucket's time, that bucket
// fires (votes mature; witness vote weight is incremented).
const DelayedVotingTotalIntervalSec uint32 = 60 * 60 * 24 * 30

// MaxDelayedVoteBuckets is the natural cap on the items vector — total
// / interval. The processor is structured so this cap is never
// exceeded as long as inputs arrive monotonically in time.
const MaxDelayedVoteBuckets = 30

// Errors mirror the messages in delayed_voting_processor.hpp.
var (
	ErrDelayedHeadTimeRegress = errors.New("head time must be greater or equal to last delayed voting time")
	ErrDelayedSumNotZero      = errors.New("sum of delayed votings must be equal to zero")
	ErrDelayedSumNotEnough    = errors.New("sum of delayed votings must be greater or equal to bucket value")
	ErrDelayedEraseTooMany    = errors.New("count of erased votes must be greater or equal to sum of delayed votings")
)

// DelayedVotingAdd appends `val` to the items collection at the
// requested head_time. If the back bucket is within
// DelayedVotingIntervalSec, val is folded into it; otherwise a new
// bucket is appended.
//
// Mirrors delayed_voting_processor::add (delayed_voting_processor.hpp).
func DelayedVotingAdd(items *[]DelayedVoteEntry, sum *uint64, headTime uint32, val uint64) error {
	if val == 0 {
		return nil
	}
	if len(*items) == 0 {
		*sum += val
		*items = append(*items, DelayedVoteEntry{Time: headTime, Val: val})
		return nil
	}
	back := &(*items)[len(*items)-1]
	if headTime < back.Time {
		return ErrDelayedHeadTimeRegress
	}
	*sum += val
	if headTime >= back.Time+DelayedVotingIntervalSec {
		*items = append(*items, DelayedVoteEntry{Time: headTime, Val: val})
	} else {
		back.Val += val
	}
	return nil
}

// DelayedVotingEraseFront drops the oldest bucket. If items is empty,
// requires sum == 0 (otherwise we lost track somewhere).
//
// Mirrors delayed_voting_processor::erase_front.
func DelayedVotingEraseFront(items *[]DelayedVoteEntry, sum *uint64) error {
	if len(*items) == 0 {
		if *sum != 0 {
			return ErrDelayedSumNotZero
		}
		return nil
	}
	front := (*items)[0]
	if *sum < front.Val {
		return ErrDelayedSumNotEnough
	}
	*sum -= front.Val
	*items = (*items)[1:]
	return nil
}

// DelayedVotingErase removes `count` votes from the back of the
// collection. Buckets are popped/decremented from the back until
// count is consumed.
//
// Mirrors delayed_voting_processor::erase.
func DelayedVotingErase(items *[]DelayedVoteEntry, sum *uint64, count uint64) error {
	if count == 0 {
		return nil
	}
	if count > *sum {
		return ErrDelayedEraseTooMany
	}
	if *sum == count {
		*sum = 0
		*items = (*items)[:0]
		return nil
	}
	*sum -= count
	for count > 0 {
		back := &(*items)[len(*items)-1]
		if count >= back.Val {
			count -= back.Val
			*items = (*items)[:len(*items)-1]
		} else {
			back.Val -= count
			count = 0
		}
	}
	return nil
}

// DelayedVotingAddOnAccount is a thin wrapper that operates on the
// account's queue + sum fields. Used by transfer_to_vesting and
// (post-HF24) other VESTS-credit paths.
func DelayedVotingAddOnAccount(a *Account, headTime uint32, val uint64) error {
	return DelayedVotingAdd(&a.DelayedVotes, &a.SumDelayedVotes, headTime, val)
}

// DelayedVotingEraseOnAccount removes `count` votes from the back of
// the account's queue. Used by withdrawals (post-HF24) to remove
// not-yet-matured votes when VESTS leave the account.
func DelayedVotingEraseOnAccount(a *Account, count uint64) error {
	return DelayedVotingErase(&a.DelayedVotes, &a.SumDelayedVotes, count)
}
