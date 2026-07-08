package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"

	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Usage: feedhist <mem.bin>
// Loads a dir-snapshot's raw K/V dump and prints the FeedHistory:
// the 84-entry PriceHistory (hourly medians), the sorted order, and the
// current_median_history (median-of-medians used for HBD conversions).
func cmdStateFeed(args []string) int {
	os.Args = append([]string{"gopherd-state-feed"}, args...)

	f, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	db := state.OpenMem()
	br := bufio.NewReader(f)
	var hdr [12]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		panic(err)
	}
	if string(hdr[:8]) != "GHIVEMEM" {
		panic(fmt.Sprintf("bad magic %q", hdr[:8]))
	}
	if err := state.ReadAllRows(db, br); err != nil {
		panic(err)
	}
	s := state.NewSession(db)
	fh, err := objects.GetFeedHistory(s)
	if err != nil {
		panic(err)
	}
	fmt.Printf("price_history len=%d\n", len(fh.PriceHistory))
	fmt.Printf("current_median_history = %d/%d\n", fh.CurrentMedianHistory.Base.Amount, fh.CurrentMedianHistory.Quote.Amount)
	fmt.Println("--- in chronological order (oldest first) ---")
	for i, p := range fh.PriceHistory {
		fmt.Printf("[%2d] %d/%d\n", i, p.Base.Amount, p.Quote.Amount)
	}
	fmt.Println("--- sorted by price (base/quote) ---")
	idx := make([]int, len(fh.PriceHistory))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool {
		pa, pb := fh.PriceHistory[idx[a]], fh.PriceHistory[idx[b]]
		// base_a/quote_a < base_b/quote_b  ->  base_a*quote_b < base_b*quote_a
		return int64(pa.Base.Amount)*int64(pb.Quote.Amount) < int64(pb.Base.Amount)*int64(pa.Quote.Amount)
	})
	for rank, i := range idx {
		mark := ""
		if rank == len(idx)/2 {
			mark = "  <-- MEDIAN"
		}
		p := fh.PriceHistory[i]
		fmt.Printf("rank %2d: [chrono %2d] %d/%d%s\n", rank, i, p.Base.Amount, p.Quote.Amount, mark)
	}
	return 0
}
