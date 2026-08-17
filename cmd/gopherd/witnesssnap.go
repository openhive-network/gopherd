package main

import (
	"fmt"
	"os"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
	"gopherd/internal/state/snapshot"
)

// Usage: witnesssnap <witness_store.bin> <name> [name...]
// Reads a snapshot's witness_store.bin directly and prints the named
// witnesses' votes + virtual-schedule fields — for localizing schedule
// divergences without a replay.
func cmdStateWitnesses(args []string) int {
	os.Args = append([]string{"gopherd-state-witnesses"}, args...)

	f, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	r, decClose, err := snapshot.MaybeCompressedReader(f)
	if err != nil {
		panic(err)
	}
	defer decClose()
	boot := state.NewSession(state.OpenMem())
	st := objects.EnsureWitnessStore(boot)
	if err := st.UnmarshalSnapshot(r); err != nil {
		panic(err)
	}
	if len(os.Args) >= 3 && os.Args[2] == "--rates" {
		// Dump top witnesses by vote with their HBDInterestRate — for
		// checking the median input set.
		type wr struct {
			name  string
			votes int64
			rate  uint16
		}
		var all []wr
		_ = st.RangePrimary(func(w *objects.Witness) (bool, error) {
			all = append(all, wr{string(w.Owner), w.TotalVotesVests, w.HBDInterestRate})
			return true, nil
		})
		// simple partial sort: print all with votes>0, caller eyeballs top-21
		for i := 1; i < len(all); i++ {
			x := all[i]
			j := i
			for ; j > 0 && all[j-1].votes < x.votes; j-- {
				all[j] = all[j-1]
			}
			all[j] = x
		}
		lim := 30
		if lim > len(all) {
			lim = len(all)
		}
		for i := 0; i < lim; i++ {
			fmt.Printf("%2d %-18s votes=%d hbd_interest_rate=%d\n", i, all[i].name, all[i].votes, all[i].rate)
		}
		return 0
	}
	for _, name := range os.Args[2:] {
		w, ok := st.Get(auth.AccountName(name))
		if !ok {
			fmt.Printf("%s: NOT FOUND\n", name)
			continue
		}
		fmt.Printf("%-16s votes=%d sched_time=%x pos=%x last_update=%x feed=%d/%d feed_ts=%d last_confirmed=%d\n",
			name, w.TotalVotesVests, w.VirtualScheduledTime, w.VirtualPosition, w.VirtualLastUpdate,
			w.HBDExchangeRate.Base.Amount, w.HBDExchangeRate.Quote.Amount, w.LastHBDExchangeUpdate, w.LastConfirmedBlockNum)
	}
	return 0
}
