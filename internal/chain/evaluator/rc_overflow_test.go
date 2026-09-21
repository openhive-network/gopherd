package evaluator

import (
	"errors"
	"math"
	"testing"
)

// TestSafeAddRC pins the fc::safe<int64_t> semantics hived relies on for RC
// arithmetic (hive commit 8cb83d0e3). Go's int64 wraps where share_type
// throws, and the wrap is exploitable: delegate_rc applies ONE max_rc to up to
// 100 delegatees, so a large enough max_rc makes the running delta_total wrap
// negative — and a negative total passes the "can the delegator afford this"
// check trivially.
func TestSafeAddRC(t *testing.T) {
	cases := []struct {
		name    string
		a, b    int64
		want    int64
		wantErr bool
	}{
		{"ordinary", 100, 200, 300, false},
		{"subtracting", 100, -40, 60, false},
		{"max plus zero", math.MaxInt64, 0, math.MaxInt64, false},
		{"min plus zero", math.MinInt64, 0, math.MinInt64, false},
		{"opposite signs never overflow", math.MaxInt64, math.MinInt64, -1, false},
		{"positive overflow", math.MaxInt64, 1, 0, true},
		{"positive overflow, both large", math.MaxInt64 / 2, math.MaxInt64/2 + 2, 0, true},
		{"negative overflow", math.MinInt64, -1, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := safeAddRC(tc.a, tc.b)
			if tc.wantErr {
				if !errors.Is(err, ErrRCOverflow) {
					t.Fatalf("got (%d, %v), want ErrRCOverflow", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSafeAddRCMatchesWrapping is the mutation guard: a plain `a + b` passes
// every non-overflow case above, so without this the test would not
// distinguish the checked helper from the unchecked addition it replaced.
func TestSafeAddRCMatchesWrapping(t *testing.T) {
	// The wrap that makes the delegate_rc total go negative.
	const perDelegatee = math.MaxInt64 / 4
	total := int64(0)
	wrapped := int64(0)
	var overflowed bool
	for i := 0; i < 100; i++ {
		wrapped += perDelegatee
		next, err := safeAddRC(total, perDelegatee)
		if err != nil {
			overflowed = true
			break
		}
		total = next
	}
	if !overflowed {
		t.Fatal("100 delegatees at MaxInt64/4 must overflow")
	}
	if wrapped >= 0 {
		t.Fatal("setup: the unchecked sum should have wrapped negative, making the affordability check pass")
	}
}
