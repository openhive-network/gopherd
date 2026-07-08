package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
)

func stripLastTab(s string) string {
	i := strings.LastIndexByte(s, '\t')
	if i < 0 {
		return s
	}
	return s[:i]
}

// cmdWitnessDiff compares two TSV files produced by GHIVEDIFF_DUMP_FILE
// (one from hived, one from ghive-tools replay) and reports the FIRST
// per-block per-witness state divergence.
//
// Each input is a tab-separated stream:
//
//	block name votes position last_update scheduled_time schedule
//
// (plus an `__ws__` row per block carrying ws.current_virtual_time
// in the scheduled_time column).
//
// Both files are scanned linearly. We compare row-by-row in lockstep
// (both must produce rows in the same block+name order); on the first
// mismatch we report it and exit. If one side runs out of rows, we
// report that.
func cmdWitnessDiff(args []string) int {
	fs := flag.NewFlagSet("witness-diff", flag.ContinueOnError)
	witness := fs.String("witness", "", "if set, only show divergences for this witness name")
	limit := fs.Int("limit", 5, "max number of divergences to show before stopping")
	ignoreSchedule := fs.Bool("ignore-schedule", true, "ignore the schedule column (it's a cosmetic field that lags by one rebuild)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: witness-diff [--witness=NAME] [--limit=N] <a.tsv> <b.tsv>")
		return 2
	}
	a, err := os.Open(rest[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", rest[0], err)
		return 1
	}
	defer a.Close()
	b, err := os.Open(rest[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", rest[1], err)
		return 1
	}
	defer b.Close()
	sa := bufio.NewScanner(a)
	sa.Buffer(make([]byte, 1<<20), 1<<20)
	sb := bufio.NewScanner(b)
	sb.Buffer(make([]byte, 1<<20), 1<<20)

	// Skip header lines if present.
	if sa.Scan() && !strings.HasPrefix(sa.Text(), "block\tname") {
		// First line was data; rewind by re-reading.
		// Workaround: re-open.
		a.Close()
		a, _ = os.Open(rest[0])
		defer a.Close()
		sa = bufio.NewScanner(a)
		sa.Buffer(make([]byte, 1<<20), 1<<20)
	}
	if sb.Scan() && !strings.HasPrefix(sb.Text(), "block\tname") {
		b.Close()
		b, _ = os.Open(rest[1])
		defer b.Close()
		sb = bufio.NewScanner(b)
		sb.Buffer(make([]byte, 1<<20), 1<<20)
	}

	diffs := 0
	lastBlock := ""
	for {
		hasA := sa.Scan()
		hasB := sb.Scan()
		if !hasA && !hasB {
			break
		}
		if !hasA {
			fmt.Printf("a-EOF before b at: %s\n", sb.Text())
			return 0
		}
		if !hasB {
			fmt.Printf("b-EOF before a at: %s\n", sa.Text())
			return 0
		}
		la, lb := sa.Text(), sb.Text()
		if *witness != "" {
			// Skip rows that don't match the watched witness.
			fa := strings.SplitN(la, "\t", 3)
			fb := strings.SplitN(lb, "\t", 3)
			if len(fa) >= 2 && fa[1] != *witness && len(fb) >= 2 && fb[1] != *witness {
				continue
			}
		}
		if la == lb {
			fa := strings.SplitN(la, "\t", 2)
			if len(fa) >= 1 {
				lastBlock = fa[0]
			}
			continue
		}
		// Optionally trim the schedule column (last) before comparing —
		// the schedule field is set at TIMESHARE/MINER pick time, persists
		// across rebuilds, and drifts cosmetically without affecting
		// scheduling correctness.
		if *ignoreSchedule {
			la2 := stripLastTab(la)
			lb2 := stripLastTab(lb)
			if la2 == lb2 {
				fa := strings.SplitN(la, "\t", 2)
				if len(fa) >= 1 {
					lastBlock = fa[0]
				}
				continue
			}
		}
		fmt.Printf("DIVERGE (last matching block before this: %s)\n", lastBlock)
		fmt.Printf("  a: %s\n", la)
		fmt.Printf("  b: %s\n", lb)
		// Show field-level diff.
		fa := strings.Split(la, "\t")
		fb := strings.Split(lb, "\t")
		if len(fa) == len(fb) {
			cols := []string{"block", "name", "votes", "position", "last_update", "scheduled_time", "schedule"}
			for i := 0; i < len(fa); i++ {
				if fa[i] != fb[i] {
					col := "?"
					if i < len(cols) {
						col = cols[i]
					}
					fmt.Printf("    %s: a=%s b=%s\n", col, fa[i], fb[i])
				}
			}
		}
		diffs++
		if diffs >= *limit {
			break
		}
	}
	if diffs == 0 {
		fmt.Println("MATCH (no divergences found)")
		return 0
	}
	return 1
}
