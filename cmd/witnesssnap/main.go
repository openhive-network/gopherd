package main

import (
	"fmt"
	"os"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Usage: witnesssnap <witness_store.bin> <name> [name...]
// Reads a snapshot's witness_store.bin directly and prints the named
// witnesses' votes + virtual-schedule fields — for localizing schedule
// divergences without a replay.
func main() {
	f, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	boot := state.NewSession(state.OpenMem())
	st := objects.EnsureWitnessStore(boot)
	if err := st.UnmarshalSnapshot(f); err != nil {
		panic(err)
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
}
