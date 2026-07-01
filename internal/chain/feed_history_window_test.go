package chain

import (
	"testing"

	"hived-go/internal/protocol/asset"
)

// TestPriceHistoryWindowStaysPeggedAt168PostHF16 pins the hived feed-history
// quirk that trimPriceHistory must reproduce: hived pops at most ONE entry per
// hourly update (database.cpp:2482), so when the HF16 window constant drops
// 168 -> 84 the already-full 168-entry deque never shrinks — it stays at 168
// for the rest of chain history. A truncate-to-window (the old Go behavior)
// would wrongly collapse it to 84, changing the median index (84 vs 42) and
// every downstream HBD payout. This was the root of the block 49,596,514 wall.
func TestPriceHistoryWindowStaysPeggedAt168PostHF16(t *testing.T) {
	// trimPriceHistory inspects only the slice length, so zero-value
	// prices are sufficient for the windowing behavior under test.
	var p asset.Price
	var hist []asset.Price
	// Pre-HF16: window = 168. Grow well past it; it must settle at exactly 168.
	for i := 0; i < 500; i++ {
		hist = append(hist, p)
		hist = trimPriceHistory(hist, feedHistoryWindowPreHF16)
	}
	if len(hist) != feedHistoryWindowPreHF16 {
		t.Fatalf("pre-HF16 window: got %d, want %d", len(hist), feedHistoryWindowPreHF16)
	}

	// HF16 fires: the window CONSTANT drops to 84, but single-pop trimming
	// must keep the deque pegged at 168 — never shrinking to 84.
	for i := 0; i < 500; i++ {
		hist = append(hist, p)
		hist = trimPriceHistory(hist, feedHistoryWindow)
	}
	if len(hist) != feedHistoryWindowPreHF16 {
		t.Fatalf("post-HF16 window must stay pegged at %d (hived pop_front quirk), got %d",
			feedHistoryWindowPreHF16, len(hist))
	}
}

// TestPriceHistoryWindowGrowsToNewCapWhenStartedSmall confirms that a deque
// which starts BELOW the post-HF16 cap grows up to 84 (not 168): the peg only
// happens because the deque was already full at 168 when the constant dropped.
func TestPriceHistoryWindowGrowsToNewCapWhenStartedSmall(t *testing.T) {
	var p asset.Price
	var hist []asset.Price
	for i := 0; i < 500; i++ {
		hist = append(hist, p)
		hist = trimPriceHistory(hist, feedHistoryWindow) // 84 from the start
	}
	if len(hist) != feedHistoryWindow {
		t.Fatalf("window started small: got %d, want %d", len(hist), feedHistoryWindow)
	}
}
