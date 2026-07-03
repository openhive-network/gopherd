package main

import (
	"fmt"
	"os"
	"sort"

	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Usage: schedsnap <witness_schedule_store.bin> <witness_store.bin>
// Prints CurrentShuffledWitnesses (the active 21) with each witness's
// vote total, sorted ascending — the lowest-vote entries are the
// timeshare/backup picks. Lets us read the scheduled timeshare witness
// at a snapshot block WITHOUT a replay, to bisect timeshare-rotation drift.
func main() {
	schedF, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer schedF.Close()
	wF, err := os.Open(os.Args[2])
	if err != nil {
		panic(err)
	}
	defer wF.Close()

	s := state.NewSession(state.OpenMem())
	ss := objects.EnsureWitnessScheduleStore(s)
	if err := ss.UnmarshalSnapshot(schedF); err != nil {
		panic(err)
	}
	ws := objects.EnsureWitnessStore(s)
	if err := ws.UnmarshalSnapshot(wF); err != nil {
		panic(err)
	}
	sched, err := objects.GetWitnessSchedule(s)
	if err != nil {
		panic(err)
	}
	type e struct {
		name  string
		votes int64
	}
	var list []e
	for _, n := range sched.CurrentShuffledWitnesses {
		v := int64(-1)
		if w, ok := ws.Get(n); ok {
			v = w.TotalVotesVests
		}
		list = append(list, e{string(n), v})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].votes < list[j].votes })
	fmt.Printf("num_scheduled=%d shuffled_len=%d\n", sched.NumScheduledWitnesses, len(sched.CurrentShuffledWitnesses))
	for _, x := range list {
		fmt.Printf("%-18s votes=%d\n", x.name, x.votes)
	}
}
