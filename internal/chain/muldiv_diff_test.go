package chain

import (
	"math/big"
	"testing"
)

// FuzzMulDivExact pins mulDiv, which every HBD/HIVE conversion and price
// calculation goes through, to exact arithmetic: whenever a*b/c fits in
// int64, the result must be the exact quotient truncated toward zero, as
// C++'s 128-bit division gives it.
func FuzzMulDivExact(f *testing.F) {
	f.Add(int64(1_000_000), int64(1_000), int64(3_000))
	f.Add(int64(9_000_000_000_000_000_000), int64(3), int64(7))
	f.Add(int64(-7), int64(3), int64(2))
	f.Add(int64(-1<<63), int64(1), int64(-1))
	f.Add(int64(1<<31), int64(1<<31), int64(3))
	f.Fuzz(func(t *testing.T, a, b, c int64) {
		if c == 0 {
			if mulDiv(a, b, c) != 0 {
				t.Fatalf("mulDiv(%d, %d, 0) != 0", a, b)
			}
			return
		}
		exact := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
		exact.Quo(exact, big.NewInt(c)) // truncates toward zero
		if !exact.IsInt64() {
			return // saturates; out of range for every caller
		}
		if got := mulDiv(a, b, c); got != exact.Int64() {
			t.Fatalf("mulDiv(%d, %d, %d) = %d, want %d", a, b, c, got, exact.Int64())
		}
	})
}
