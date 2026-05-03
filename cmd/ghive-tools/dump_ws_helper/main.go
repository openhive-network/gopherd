package main

import (
    "fmt"
    "os"
    "sort"

    "hived-go/internal/state"
    "hived-go/internal/state/objects"
)

func main() {
    db := state.OpenMem()
    if _, err := db.LoadSnapshot(os.Args[1]); err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
    s := state.NewSession(db)
    ws, err := objects.GetWitnessSchedule(s)
    if err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
    var sched []string
    for _, n := range ws.CurrentShuffledWitnesses {
        if string(n) != "" {
            sched = append(sched, string(n))
        }
    }
    sort.Strings(sched)
    fmt.Printf("WS at snapshot:\n  num_scheduled=%d\n  shuffled (sorted)=%v\n",
        ws.NumScheduledWitnesses, sched)
    fmt.Printf("  current_virtual_time=%x\n", ws.CurrentVirtualTime)
}
