package state

import (
	"bytes"
	"math"
	"sort"
	"testing"
)

// Property: AppendUint*BE produces a byte sequence whose byte-wise
// comparison agrees with numeric comparison.
func TestUint32BEByteOrderMatchesNumeric(t *testing.T) {
	values := []uint32{0, 1, 0x7f, 0x80, 0xffff, 0x1_0000, math.MaxUint32 - 1, math.MaxUint32}
	encoded := make([][]byte, len(values))
	for i, v := range values {
		encoded[i] = AppendUint32BE(nil, v)
	}
	// Encoded list should already be in ascending byte order.
	for i := 1; i < len(encoded); i++ {
		if bytes.Compare(encoded[i-1], encoded[i]) >= 0 {
			t.Fatalf("uint32 BE byte order violated: %v vs %v", encoded[i-1], encoded[i])
		}
	}
}

func TestInt64BESignFlipMatchesNumericOrder(t *testing.T) {
	values := []int64{math.MinInt64, -1_000_000, -1, 0, 1, 1_000_000, math.MaxInt64}
	encoded := make([][]byte, len(values))
	for i, v := range values {
		encoded[i] = AppendInt64BE(nil, v)
	}
	// Encoded slices should already match the numeric ordering.
	for i := 1; i < len(encoded); i++ {
		if bytes.Compare(encoded[i-1], encoded[i]) >= 0 {
			t.Fatalf("int64 BE sign-flip order violated: v=%d e=%x and v=%d e=%x",
				values[i-1], encoded[i-1], values[i], encoded[i])
		}
	}

	// Sort a shuffled list and confirm we recover the original numeric order.
	shuffled := append([]int64{}, values...)
	for i := range shuffled {
		j := (i*7 + 3) % len(shuffled)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	encShuffled := make([][]byte, len(shuffled))
	for i, v := range shuffled {
		encShuffled[i] = AppendInt64BE(nil, v)
	}
	sort.Slice(encShuffled, func(i, j int) bool { return bytes.Compare(encShuffled[i], encShuffled[j]) < 0 })
	for i := range encShuffled {
		want := AppendInt64BE(nil, values[i])
		if !bytes.Equal(encShuffled[i], want) {
			t.Fatalf("sort mismatch at %d: got %x, want %x", i, encShuffled[i], want)
		}
	}
}

func TestAccountNameFixedWidth(t *testing.T) {
	// Short names must be NUL-padded; long names truncate to 16.
	short := AppendAccountName(nil, "alice")
	if len(short) != AccountNameKeyLen {
		t.Fatalf("short width: %d", len(short))
	}
	if string(short[:5]) != "alice" || short[5] != 0 || short[15] != 0 {
		t.Fatalf("short padding: %x", short)
	}
	long := AppendAccountName(nil, "abcdefghijklmnopqrstuv")
	if len(long) != AccountNameKeyLen {
		t.Fatalf("long width: %d", len(long))
	}
	if string(long) != "abcdefghijklmnop" {
		t.Fatalf("long truncation: %q", long)
	}
}

func TestAccountNameByteOrderMatchesString(t *testing.T) {
	// "alice" < "alicia" < "bob" lexically and as state keys.
	names := []string{"alice", "alicia", "bob", "carol", "z"}
	encoded := make([][]byte, len(names))
	for i, n := range names {
		encoded[i] = AppendAccountName(nil, n)
	}
	for i := 1; i < len(encoded); i++ {
		if bytes.Compare(encoded[i-1], encoded[i]) >= 0 {
			t.Fatalf("name order violated: %s vs %s", names[i-1], names[i])
		}
	}
}

func TestFlipForDescending(t *testing.T) {
	a := AppendUint32BE(nil, 1)
	b := AppendUint32BE(nil, 1000)
	if bytes.Compare(a, b) >= 0 {
		t.Fatal("setup: 1 should sort below 1000 when ascending")
	}
	if bytes.Compare(FlipForDescending(a), FlipForDescending(b)) <= 0 {
		t.Fatal("FlipForDescending should reverse the order")
	}
}
