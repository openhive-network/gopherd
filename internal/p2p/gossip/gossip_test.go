package gossip

import (
	"sync"
	"testing"

	"hived-go/internal/protocol/block"
)

// TestOnBlockReceivedIsNoOp pins the regression: gossip's
// OnBlockReceived must NOT touch chain.Database or trigger any apply
// path. The pre-existing race between gossip's apply loop and sync's
// applied to chain.Database's shared *state.Session — fixed by
// deleting gossip's apply loop entirely. This test makes sure nobody
// re-introduces it: with a nil Node we can't broadcast, with no
// Mempool/Chain wired we can't apply, and the call must still return
// silently.
func TestOnBlockReceivedIsNoOp(t *testing.T) {
	s := New(Config{
		// Intentionally nil — anything that tried to apply or broadcast
		// would nil-panic.
		Node:          nil,
		DedupCapacity: 64,
	})

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := make([]byte, 80+block.IDSize)
			body[0] = byte(i)
			s.OnBlockReceived(nil, body)
		}(i)
	}
	wg.Wait()
}

// TestStartStopIsNoOp is the secondary regression pin: gossip no
// longer owns any background goroutines, so Start/Stop must be safe to
// call (or omit) and never deadlock. Old code spun up an applyLoop
// goroutine and Stop blocked on its drain channel.
func TestStartStopIsNoOp(t *testing.T) {
	s := New(Config{Node: nil})
	// Multiple Start/Stop calls are fine.
	s.Start()
	s.Stop()
	s.Start()
	s.Start()
	s.Stop()
	s.Stop()
}

// TestSeenBlkDedupesAcrossBroadcastPaths verifies that when a block ID
// is marked seen via one broadcast path, the other path skips it. This
// is what prevents witness's BroadcastBlock + sync's
// BroadcastAppliedBlock callback from double-broadcasting the SAME
// locally-produced block (BroadcastBlock fires from witness BEFORE
// returning to chain.ApplyBlock; the OnBlockApplied hook then fires
// inside ApplyBlock with the same body).
//
// We reach into the seenSet directly (it's package-private) instead of
// going through Broadcast, since wiring a real Node + counting calls
// would require a transport that's gratuitous for this assertion.
func TestSeenBlkDedupesAcrossBroadcastPaths(t *testing.T) {
	s := New(Config{Node: nil, DedupCapacity: 16})
	var id block.ID
	id[0] = 0xaa
	if !s.seenBlk.add(id) {
		t.Fatal("first add should be true")
	}
	if s.seenBlk.add(id) {
		t.Fatal("second add of same ID should be false (dedup)")
	}
}
