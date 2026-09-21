package sync

import (
	"sync/atomic"
	"time"
)

// Timestamp gate for inbound blocks, ported from chain_plugin::check_time_in_block
// (hive 1.29.0-rc2, commits bd9738037 and 0b6f8d091).
//
// gopherd had no timestamp gate at all: a block claiming any future timestamp
// went straight onto the apply queue. hived has rejected blocks more than
// allowFutureTime ahead since forever, and 1.29 splits the window in two.
//
// The reason for the split is that a block a little ahead of our clock is not
// invalid, it is EARLY — either the producer's clock is fast or ours is slow.
// Witnesses deliberately produce up to 500ms before their slot, and hived's
// old comparison truncated to whole seconds, so the effective tolerance was
// smaller than it looked. Applying such a block immediately lets a fast-clock
// witness leapfrog the witness scheduled ahead of it: we apply the early
// block, the honest witness's block arrives as a same-height fork nobody
// switches to, and the HONEST witness is charged the miss. Holding the early
// block instead means that if the preceding block shows up during the hold it
// is applied first, and the fast-clock witness takes the miss it earned.
const (
	// blockAcceptLeadTime — a block at most this far ahead of the local
	// clock is queued immediately.
	blockAcceptLeadTime = time.Second
	// allowFutureTime — beyond blockAcceptLeadTime but within this, a block
	// is held until it comes within blockAcceptLeadTime. Beyond it, refused.
	allowFutureTime = 5 * time.Second
	// maxHeldBlocks bounds concurrent holds. Each hold pins a goroutine and
	// the block body for up to allowFutureTime-blockAcceptLeadTime, and the
	// block is validated only AFTER the hold, so without a bound a peer could
	// park arbitrary memory by flooding cheaply fabricated future-timestamped
	// blocks — and the scheduled witness can equivocate, so validating before
	// the hold would not close it either. The honest network essentially
	// never holds more than one block at a time (one block per 3s slot, and a
	// hold at all means the producer is over a second fast), so 4 loses
	// nothing.
	maxHeldBlocks = 4
)

// earlyBlockVerdict says what the gate decided about one inbound block.
type earlyBlockVerdict int

const (
	blockOnTime  earlyBlockVerdict = iota // queue it now
	blockHeld                             // held, then queue it
	blockRefused                          // too far ahead, or the hold slots are full
)

// earlyBlockGate implements the timestamp window. The clock and the sleep are
// injectable so tests do not have to wait in real time.
type earlyBlockGate struct {
	held atomic.Int32
	now  func() time.Time
	// sleep is called with the hold duration. It returns false if the node is
	// shutting down, in which case the block is dropped rather than queued —
	// hived throws canceled_exception at the same point.
	sleep func(time.Duration) bool
}

func newEarlyBlockGate(stopCh <-chan struct{}) *earlyBlockGate {
	return &earlyBlockGate{
		now: time.Now,
		sleep: func(d time.Duration) bool {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return true
			case <-stopCh:
				return false
			}
		},
	}
}

// check applies the window to a block timestamp (unix seconds, as carried in
// the block header). It BLOCKS for the hold, so it must be called on the
// receiving goroutine and never on the single apply goroutine: the whole point
// is that the preceding witness's block can be applied while this one waits.
//
// A zero timestamp means the header could not be parsed; leave that to the
// apply path, which produces the real error.
func (g *earlyBlockGate) check(blockTimestamp uint32) earlyBlockVerdict {
	if blockTimestamp == 0 {
		return blockOnTime
	}
	now := g.now()
	blockTime := time.Unix(int64(blockTimestamp), 0)
	// Microsecond resolution, not whole seconds: the truncation is what made
	// the old tolerance larger than intended.
	if blockTime.After(now.Add(allowFutureTime)) {
		return blockRefused
	}
	releaseTime := blockTime.Add(-blockAcceptLeadTime)
	if !releaseTime.After(now) {
		return blockOnTime
	}
	if g.held.Add(1) > maxHeldBlocks {
		g.held.Add(-1)
		// Same treatment as a too-far-future block, i.e. the pre-hold
		// behaviour. Under a flood an attacker is back to that exposure after
		// at most maxHeldBlocks parked blocks.
		return blockRefused
	}
	defer g.held.Add(-1)
	if !g.sleep(releaseTime.Sub(now)) {
		return blockRefused
	}
	return blockHeld
}
