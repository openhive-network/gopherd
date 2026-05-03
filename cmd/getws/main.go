package main
import (
  "fmt"
  "os"
  "hived-go/internal/state"
  "hived-go/internal/state/objects"
)
func main() {
  db := state.OpenMem()
  if _, err := db.LoadSnapshot(os.Args[1]); err != nil { panic(err) }
  s := state.NewSession(db)
  ws, err := objects.GetWitnessSchedule(s)
  if err != nil { panic(err) }
  dgpo, err := objects.GetDGPO(s)
  if err != nil { panic(err) }
  fmt.Printf("num_scheduled=%d max_voted=%d max_miner=%d max_runner=%d num_pow_witnesses=%d total_pow=%d head=%d\n",
    ws.NumScheduledWitnesses, ws.MaxVotedWitnesses, ws.MaxMinerWitnesses, ws.MaxRunnerWitnesses,
    dgpo.NumPowWitnesses, dgpo.TotalPow, dgpo.HeadBlockNumber)
}
