package main

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"time"

	"hived-go/internal/blocklog"
)

// cmdCompareBlocklog byte-compares blocks in [from,to] between a reference
// block_log and another (e.g. a live daemon's block_log). Both readers
// return the canonical decompressed signed_block bytes, so this works even
// when one log is compressed and the other isn't. Prints the first mismatch
// or confirms the range is byte-identical.
//
//	ghive-tools compare-blocklog <ref> <other> [from] [to]
//
// Exit 0 = identical over the compared range, 1 = mismatch or error.
func cmdCompareBlocklog(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: gopherd compare-blocklog <ref> <other> [from] [to]")
		return 2
	}
	refPath, otherPath := args[0], args[1]

	ref, err := blocklog.Open(refPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compare-blocklog: open ref %s: %v\n", refPath, err)
		return 1
	}
	defer ref.Close()
	other, err := blocklog.Open(otherPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compare-blocklog: open other %s: %v\n", otherPath, err)
		return 1
	}
	defer other.Close()

	from := uint32(1)
	to := other.Head()
	if ref.Head() < to {
		to = ref.Head()
	}
	if len(args) >= 3 {
		if v, err := strconv.ParseUint(args[2], 10, 32); err == nil {
			from = uint32(v)
		}
	}
	if len(args) >= 4 {
		if v, err := strconv.ParseUint(args[3], 10, 32); err == nil && uint32(v) < to {
			to = uint32(v)
		}
	}
	if from < 1 {
		from = 1
	}
	if to < from {
		fmt.Printf("compare-blocklog: nothing to compare (from=%d > to=%d; ref head=%d other head=%d)\n",
			from, to, ref.Head(), other.Head())
		return 0
	}

	start := time.Now()
	for n := from; n <= to; n++ {
		a, err := ref.Block(n)
		if err != nil {
			fmt.Fprintf(os.Stderr, "compare-blocklog: ref block %d: %v\n", n, err)
			return 1
		}
		b, err := other.Block(n)
		if err != nil {
			fmt.Fprintf(os.Stderr, "compare-blocklog: other block %d: %v\n", n, err)
			return 1
		}
		if !bytes.Equal(a, b) {
			fmt.Printf("MISMATCH at block %d: ref=%d bytes, other=%d bytes\n", n, len(a), len(b))
			return 1
		}
	}
	dt := time.Since(start).Seconds()
	rate := 0.0
	if dt > 0 {
		rate = float64(to-from+1) / dt
	}
	fmt.Printf("OK: blocks %d..%d byte-identical (%d blocks, %.0f blk/s)\n", from, to, to-from+1, rate)
	return 0
}
