package objects

import (
	"math"
	"testing"
)

// hived calculate_downvote_manabar (util/manabar.cpp, 366bc0533).
func TestDownvoteManaDelta(t *testing.T) {
	// Small amounts: both forms agree, sign inverted, truncated toward zero.
	for _, v := range []int64{0, 1, 3, 39_999, 1_000_000, -1_000_000, -3} {
		old, fixed := downvoteManaDelta(v, 2500, false), downvoteManaDelta(v, 2500, true)
		if old != fixed || old != (-v*2500)/10000 {
			t.Fatalf("v=%d: old=%d fixed=%d", v, old, fixed)
		}
	}
	// 4e15 VESTS-satoshi at 25 %: v × 2500 = 1e19 > 2^63. Pre-HF29 wraps,
	// as hived's history does; from HF29 it is exactly -1e15.
	big := int64(4_000_000_000_000_000)
	if got := downvoteManaDelta(big, 2500, true); got != -1_000_000_000_000_000 {
		t.Fatalf("HF29: got %d, want -1e15", got)
	}
	wrapped := int64(uint64(0)-uint64(big)) * 2500 / 10000 // hived's historical path
	if got := downvoteManaDelta(big, 2500, false); got != wrapped || got == -1_000_000_000_000_000 {
		t.Fatalf("pre-HF29: got %d, want the wrapped %d", got, wrapped)
	}
	// INT64_MIN: the magnitude is taken unsigned, so no overflow.
	if got := downvoteManaDelta(math.MinInt64, 5000, true); got != int64(uint64(1)<<62) {
		t.Fatalf("MinInt64 at 50%%: got %d", got)
	}
}
