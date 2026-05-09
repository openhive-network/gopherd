package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"

	"hived-go/internal/blocklog"
	"hived-go/internal/chain"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
)

// Usage: dump_witnesses <block_log> <up_to_block>
// Replays to <up_to_block>, then dumps every witness with non-zero votes
// or non-zero pow_worker, sorted by (votes desc, then pow flag).
func main() {
	logPath := os.Args[1]
	stop, _ := strconv.Atoi(os.Args[2])

	stateDB := state.OpenMem()
	if err := genesis.Apply(stateDB); err != nil {
		_ = err
	}
	cdb := chain.New(stateDB)

	r, err := blocklog.Open(logPath)
	if err != nil {
		panic(err)
	}
	defer r.Close()

	for n := uint32(1); n <= uint32(stop); n++ {
		raw, err := r.Block(n)
		if err != nil {
			continue
		}
		_, err = cdb.ApplyBlock(raw, n, chain.ApplyOptions{
			Sync:                      false,
			SkipTransactionSignatures: true,
			SkipBlockSignature:        true,
		}, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "apply block %d: %v\n", n, err)
		}
	}

	type entry struct {
		name      string
		votes     int64
		pow       uint32
		hbdRate   string
		schedKind int
		vstHex    string
	}
	var entries []entry

	sess := state.NewSession(stateDB)
	tmpDgpo, _ := objects.GetDGPO(sess)
	fmt.Printf("DEBUG dgpo.HeadBlockNumber=%d, dgpo.Time=%d, dgpo.TotalPow=%d, dgpo.NumPowWitnesses=%d, dgpo.CurrentAslot=%d\n", tmpDgpo.HeadBlockNumber, tmpDgpo.Time, tmpDgpo.TotalPow, tmpDgpo.NumPowWitnesses, tmpDgpo.CurrentAslot)
	wstore := objects.EnsureWitnessStore(sess)
	for _, n := range []string{"gxt-1080-sc-0027", "gxt-1080-sc-0028", "gxt-1080-sc-0031", "rabbit-13", "rabbit-18", "rabbit-19"} {
		w, ok := wstore.Get(auth.AccountName(n))
		if ok {
			fmt.Printf("PROBE %s pow=%d votes=%d sigkey=%x lastconf=%d totalmissed=%d\n", n, w.PowWorker, w.TotalVotesVests, w.BlockSigningKey, w.LastConfirmedBlockNum, w.TotalMissedBlocks)
		}
	}
	fmt.Println("=== ALL POW WITNESSES (sorted by pow_worker) ===")
	type pwEnt struct {
		owner string
		pow   uint64
	}
	var pwAll []pwEnt
	_ = wstore.RangePrimary(func(w *objects.Witness) (bool, error) {
		if w.PowWorker > 0 {
			pwAll = append(pwAll, pwEnt{owner: string(w.Owner), pow: w.PowWorker})
		}
		return true, nil
	})
	sort.Slice(pwAll, func(i, j int) bool { return pwAll[i].pow < pwAll[j].pow })
	fmt.Printf("count=%d\n", len(pwAll))
	for _, p := range pwAll {
		fmt.Printf("%d %s\n", p.pow, p.owner)
	}
	fmt.Printf("\n=== top-10 by PoW (smallest first, after drain) ===\n")
	powCount := 0
	_ = wstore.RangeByPow(func(w *objects.Witness) (bool, error) {
		if powCount >= 10 {
			return false, nil
		}
		fmt.Printf("\t%s\tpow=%d\trate=%d/%d\n", w.Owner, w.PowWorker, w.HBDExchangeRate.Base.Amount, w.HBDExchangeRate.Quote.Amount)
		powCount++
		return true, nil
	})
	fmt.Println()
	_ = wstore.RangeByVote(func(w *objects.Witness) (bool, error) {
		// no filter — list everything
		schedK := 99
		switch w.Schedule {
		case objects.WitnessScheduleElected:
			schedK = 0
		case objects.WitnessScheduleTimeshare:
			schedK = 1
		case objects.WitnessScheduleMiner:
			schedK = 2
		case objects.WitnessScheduleNone:
			schedK = 3
		}
		entries = append(entries, entry{
			name:      string(w.Owner),
			votes:     w.TotalVotesVests,
			pow:       uint32(w.PowWorker),
			hbdRate:   fmt.Sprintf("%d/%d", w.HBDExchangeRate.Base.Amount, w.HBDExchangeRate.Quote.Amount),
			schedKind: schedK,
			vstHex:    fmt.Sprintf("%x", w.VirtualScheduledTime),
		})
		return true, nil
	})
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].votes != entries[j].votes {
			return entries[i].votes > entries[j].votes
		}
		return entries[i].name < entries[j].name
	})
	ws, _ := objects.GetWitnessSchedule(sess)
	fmt.Printf("=== schedule at block %d ===\n", stop)
	fmt.Printf("num_scheduled=%d virtual_time=%x\n", ws.NumScheduledWitnesses, ws.CurrentVirtualTime)
	fmt.Printf("shuffled: ")
	for _, n := range ws.CurrentShuffledWitnesses {
		fmt.Printf("%s,", n)
	}
	fmt.Println()
	dgpo, _ := objects.GetDGPO(sess)
	fmt.Printf("dgpo.NumPowWitnesses=%d\n", dgpo.NumPowWitnesses)
	fmt.Printf("\n=== witnesses with votes>0 OR pow>0 (top 35 by votes) ===\n")
	fmt.Printf("rank\tname\tvotes\tpow\trate\tsched\tvst\n")
	for i, e := range entries {
		if i >= 35 {
			break
		}
		fmt.Printf("%d\t%s\t%d\t%d\t%s\t%d\t%s\n",
			i+1, e.name, e.votes, e.pow, e.hbdRate, e.schedKind, e.vstHex)
	}
	_ = auth.AccountName("")
}
