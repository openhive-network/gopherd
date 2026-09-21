package sync

import (
	"sync"
	"testing"
	"time"
)

// newTestGate returns a gate with a frozen clock and a sleep that records
// instead of waiting, so the window can be probed at microsecond precision
// without spending wall-clock time.
func newTestGate(now time.Time) (*earlyBlockGate, *[]time.Duration) {
	var mu sync.Mutex
	slept := []time.Duration{}
	g := &earlyBlockGate{
		now: func() time.Time { return now },
		sleep: func(d time.Duration) bool {
			mu.Lock()
			slept = append(slept, d)
			mu.Unlock()
			return true
		},
	}
	return g, &slept
}

// TestEarlyBlockWindow walks the three regions of
// chain_plugin::check_time_in_block's window.
func TestEarlyBlockWindow(t *testing.T) {
	// A whole second boundary so "block timestamp" (second resolution on the
	// wire) and the local clock can be offset precisely.
	now := time.Unix(1_700_000_000, 0)

	cases := []struct {
		name string
		ts   int64
		want earlyBlockVerdict
		hold time.Duration
	}{
		{"in the past", now.Unix() - 3, blockOnTime, 0},
		{"exactly now", now.Unix(), blockOnTime, 0},
		{"1s ahead is still immediate", now.Unix() + 1, blockOnTime, 0},
		{"2s ahead is held for 1s", now.Unix() + 2, blockHeld, time.Second},
		{"5s ahead is held for 4s", now.Unix() + 5, blockHeld, 4 * time.Second},
		{"6s ahead is refused", now.Unix() + 6, blockRefused, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, slept := newTestGate(now)
			if got := g.check(uint32(tc.ts)); got != tc.want {
				t.Fatalf("verdict: got %v, want %v", got, tc.want)
			}
			if tc.hold == 0 {
				if len(*slept) != 0 {
					t.Fatalf("expected no hold, slept %v", *slept)
				}
				return
			}
			if len(*slept) != 1 || (*slept)[0] != tc.hold {
				t.Fatalf("hold: got %v, want [%v]", *slept, tc.hold)
			}
		})
	}
}

// TestEarlyBlockUnparsableTimestampPasses — a zero timestamp means the header
// did not parse. The apply path produces the real error for that; the gate
// must not turn it into a silent drop with a misleading reason.
func TestEarlyBlockUnparsableTimestampPasses(t *testing.T) {
	g, _ := newTestGate(time.Unix(1_700_000_000, 0))
	if got := g.check(0); got != blockOnTime {
		t.Fatalf("got %v, want blockOnTime", got)
	}
}

// TestEarlyBlockHoldSlotsAreBounded covers hived 0b6f8d091: each hold pins a
// goroutine and a block body for up to 4 seconds, and the block is validated
// only AFTER the hold, so a peer that floods fabricated future-timestamped
// blocks could otherwise park unbounded memory. Past the cap a block is
// refused exactly as a too-far-future one is.
func TestEarlyBlockHoldSlotsAreBounded(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	release := make(chan struct{})
	entered := make(chan struct{}, maxHeldBlocks+1)
	g := &earlyBlockGate{
		now: func() time.Time { return now },
		sleep: func(time.Duration) bool {
			entered <- struct{}{}
			<-release
			return true
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < maxHeldBlocks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := g.check(uint32(now.Unix() + 3)); got != blockHeld {
				t.Errorf("filling hold slots: got %v, want blockHeld", got)
			}
		}()
	}
	for i := 0; i < maxHeldBlocks; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for hold slots to fill")
		}
	}

	// Every slot taken: the next early block is refused rather than parked.
	if got := g.check(uint32(now.Unix() + 3)); got != blockRefused {
		t.Errorf("with all %d slots held: got %v, want blockRefused", maxHeldBlocks, got)
	}
	// A block that needs no hold is unaffected by the cap.
	if got := g.check(uint32(now.Unix())); got != blockOnTime {
		t.Errorf("on-time block with all slots held: got %v, want blockOnTime", got)
	}

	close(release)
	wg.Wait()

	// Slots are returned, so the gate recovers.
	g.sleep = func(time.Duration) bool { return true }
	if got := g.check(uint32(now.Unix() + 3)); got != blockHeld {
		t.Errorf("after the holds released: got %v, want blockHeld", got)
	}
}

// TestEarlyBlockShutdownDropsHeldBlock — hived throws canceled_exception when
// the write processor stops while a block is parked. Queueing it after
// shutdown would push work at a chain that is closing.
func TestEarlyBlockShutdownDropsHeldBlock(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := &earlyBlockGate{
		now:   func() time.Time { return now },
		sleep: func(time.Duration) bool { return false },
	}
	if got := g.check(uint32(now.Unix() + 3)); got != blockRefused {
		t.Fatalf("got %v, want blockRefused", got)
	}
	if n := g.held.Load(); n != 0 {
		t.Errorf("hold slot leaked on shutdown: held=%d", n)
	}
}
