package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"sort"

	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// dump_ws_helper dumps witness schedule + per-witness votes/VSTs from a
// ghive state snapshot file. Used to diff against C++ at the same block.
//
// Usage: dump_ws_helper <snapshot-path> [topN]
func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: dump_ws_helper <snapshot-path> [topN]")
		os.Exit(2)
	}
	topN := 30
	if len(os.Args) >= 3 {
		fmt.Sscanf(os.Args[2], "%d", &topN)
	}

	db := state.OpenMem()
	head, err := db.LoadSnapshot(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("# loaded snapshot at head=%d\n", head)

	s := state.NewSession(db)
	ws, _ := objects.GetWitnessSchedule(s)
	var sched []string
	for _, n := range ws.CurrentShuffledWitnesses {
		if string(n) != "" {
			sched = append(sched, string(n))
		}
	}
	sort.Strings(sched)
	fmt.Printf("WS\n  num_scheduled=%d\n  current_virtual_time=%s\n",
		ws.NumScheduledWitnesses, hex.EncodeToString(ws.CurrentVirtualTime[:]))
	fmt.Printf("  shuffled(sorted)=%v\n", sched)

	// Walk every witness, collect votes/VST/last_update/position.
	type wDump struct {
		name           string
		votes          int64
		vst            [16]byte
		virtualLastUpd [16]byte
		virtualPos     [16]byte
		schedule       objects.WitnessScheduleType
	}
	var all []wDump
	_ = s.Range(objects.WitnessTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			w, err := objects.ReadWitness(value)
			if err != nil {
				return true, nil
			}
			all = append(all, wDump{
				name:           string(w.Owner),
				votes:          w.TotalVotesVests,
				vst:            w.VirtualScheduledTime,
				virtualLastUpd: w.VirtualLastUpdate,
				virtualPos:     w.VirtualPosition,
				schedule:       w.Schedule,
			})
			return true, nil
		})

	// Top N by votes.
	sort.Slice(all, func(i, j int) bool { return all[i].votes > all[j].votes })
	fmt.Printf("\nTOP %d by votes:\n", topN)
	for i := 0; i < topN && i < len(all); i++ {
		w := all[i]
		fmt.Printf("  %3d %-20s votes=%-25d vst=%s sched=%d\n",
			i+1, w.name, w.votes, hex.EncodeToString(w.vst[:]), w.schedule)
	}

	// Top N by VST (smallest first — these are the timeshare candidates).
	sort.Slice(all, func(i, j int) bool {
		// Skip zero VST (not in by_schedule_time index).
		zi, zj := isZero(all[i].vst), isZero(all[j].vst)
		if zi != zj {
			return !zi // non-zero first
		}
		return string(all[i].vst[:]) < string(all[j].vst[:])
	})
	fmt.Printf("\nTOP %d by smallest VST (timeshare candidates):\n", topN)
	for i := 0; i < topN && i < len(all); i++ {
		w := all[i]
		if isZero(w.vst) {
			break
		}
		fmt.Printf("  %3d %-20s votes=%-25d vst=%s last_upd=%s pos=%s\n",
			i+1, w.name, w.votes,
			hex.EncodeToString(w.vst[:]),
			hex.EncodeToString(w.virtualLastUpd[:]),
			hex.EncodeToString(w.virtualPos[:]))
	}
}

func isZero(b [16]byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
