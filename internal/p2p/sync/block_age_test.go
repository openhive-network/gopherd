package sync

import (
	"testing"
	"time"
)

// TestBlockAgeSignedNotWrapped covers a signature-verification bypass that
// lived in the apply path.
//
// The historical-catch-up flag turns OFF block-signature checking,
// transaction-signature checking and per-op failure strictness, because a
// replay of old blocks has to reproduce hived's no-validate session. It was
// computed as:
//
//	uint32(time.Now().Unix()) - ts > 5*60
//
// which WRAPS for any future timestamp — one second ahead yields 4294967295 —
// so a block dated in the future was classified as ancient history and applied
// with every signature check disabled. Any peer could have had a forged block
// accepted simply by dating it one second ahead.
//
// Signatures are skipped for blocks we are REPLAYING. A future-dated block is
// the opposite of old, so the comparison must be signed.
func TestBlockAgeSignedNotWrapped(t *testing.T) {
	now := time.Now().Unix()

	cases := []struct {
		name           string
		offset         int64 // seconds relative to now; negative = future
		wantHistorical bool
	}{
		{"an hour old is catch-up", 3600, true},
		{"just past the threshold", historicalAgeSeconds + 2, true},
		{"just inside the threshold", historicalAgeSeconds - 2, false},
		{"current", 0, false},
		{"one second in the future", -1, false},
		{"three seconds in the future", -3, false},
		{"a day in the future", -86400, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := uint32(now - tc.offset)
			if got := isHistoricalBlock(ts); got != tc.wantHistorical {
				t.Fatalf("isHistoricalBlock(now%+ds) = %v, want %v — a future block "+
					"classified as historical is applied with signature checks OFF",
					-tc.offset, got, tc.wantHistorical)
			}
		})
	}
}

// TestBlockAgeUnparsableHeaderIsStrict — a zero timestamp means the header did
// not parse. It must NOT be treated as historical: the apply path has to stay
// strict so it produces the real decode error rather than waving the block
// through with checks disabled.
func TestBlockAgeUnparsableHeaderIsStrict(t *testing.T) {
	if isHistoricalBlock(0) {
		t.Error("an unparsable header must not disable signature verification")
	}
	if got := blockAgeSeconds(0); got != 0 {
		t.Errorf("blockAgeSeconds(0) = %d, want 0 (neither old nor future)", got)
	}
}

// TestBlockAgeFutureIsNegative states the property the fix depends on, so a
// future rewrite back to unsigned arithmetic fails here rather than silently
// reopening the bypass.
func TestBlockAgeFutureIsNegative(t *testing.T) {
	future := uint32(time.Now().Unix() + 30)
	if age := blockAgeSeconds(future); age >= 0 {
		t.Errorf("blockAgeSeconds(future) = %d, want negative", age)
	}
}
