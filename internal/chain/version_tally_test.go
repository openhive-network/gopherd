package chain

import "testing"

// hived canonical_version_num (witness_schedule.cpp, 857016bfc).
func TestCanonicalVersionNum(t *testing.T) {
	for _, tc := range []struct{ in, want uint32 }{
		{0, 0},                                   // never reported
		{28 << 16, 1<<24 | 28<<16},               // 0.28.0 vote -> 1.28.0
		{28<<16 | 9, 1<<24 | 28<<16 | 9},         // 0.28.9 -> 1.28.9
		{1<<24 | 28<<16 | 8, 1<<24 | 28<<16 | 8}, // 1.N unchanged
		{2<<24 | 1<<16, 2<<24 | 1<<16},           // other majors unchanged
	} {
		if got := canonicalVersionNum(tc.in); got != tc.want {
			t.Errorf("canonicalVersionNum(%#x) = %#x, want %#x", tc.in, got, tc.want)
		}
	}
}
