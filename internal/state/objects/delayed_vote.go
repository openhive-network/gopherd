package objects

// DelayedVoteEntry mirrors C++ delayed_votes_data — a {time, val} pair
// representing a "bucket" of VESTS that arrived during a one-day window
// and is waiting out the 30-day maturation before becoming voteable.
//
// Defined in libraries/chain/include/hive/chain/util/
// delayed_voting_processor.hpp.
type DelayedVoteEntry struct {
	Time uint32
	Val  uint64
}
