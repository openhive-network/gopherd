package main

// replay runs the apply pipeline against a block_log: genesis state →
// state.Database → chain.Database.ApplyBlock per block → final summary.
//
// Output is intentionally compact: progress every N blocks, a final
// "examined / errors / rate / head" summary, and the resulting DGPO so
// callers can compare against `hived state_snapshot` output.

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"hived-go/internal/blocklog"
	"hived-go/internal/chain"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
)

func cmdReplay(args []string) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "Pebble data dir (defaults to a tempdir; pass a real path to persist across runs)")
	startBlock := fs.Uint("start", 1, "first block to apply")
	count := fs.Uint("count", 0, "number of blocks to apply (0 = all up to head)")
	progressEvery := fs.Uint("progress-every", 10_000, "print progress every N blocks")
	maxErrors := fs.Uint("max-errors", 5, "abort after this many apply errors")
	tolerateFailing := fs.Bool("tolerate-failing-ops", false, "skip ops whose evaluator returns an error (use during P1 incremental development; never on a real node)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: ghive-tools replay [flags] <block_log_path>")
		return 2
	}
	logPath := rest[0]

	dir := *dataDir
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "ghive-replay-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "replay: tempdir: %v\n", err)
			return 1
		}
		defer os.RemoveAll(dir)
		fmt.Printf("replay: using ephemeral state dir %s\n", dir)
	} else {
		fmt.Printf("replay: state dir %s\n", dir)
	}

	stateDB, err := state.Open(dir, 256<<20) // 256 MB Pebble cache
	if err != nil {
		fmt.Fprintf(os.Stderr, "replay: open state: %v\n", err)
		return 1
	}
	defer stateDB.Close()

	if err := genesis.Apply(stateDB); err != nil {
		fmt.Fprintf(os.Stderr, "replay: genesis: %v\n", err)
		return 1
	}

	bl, err := blocklog.Open(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "replay: open block_log: %v\n", err)
		return 1
	}
	defer bl.Close()

	end := uint32(*startBlock) + uint32(*count) - 1
	if *count == 0 || end > bl.Head() {
		end = bl.Head()
	}
	fmt.Printf("replay: applying %d..%d (head=%d)\n", *startBlock, end, bl.Head())

	cdb := chain.New(stateDB)
	start := time.Now()
	var (
		applied, errCount uint64
		stats             chain.Stats
	)
	opts := chain.ApplyOptions{Sync: false, SkipUnknownOps: false, TolerateFailingOps: *tolerateFailing}

	for n := uint32(*startBlock); n <= end; n++ {
		raw, err := bl.Block(n)
		if err != nil {
			fmt.Fprintf(os.Stderr, "block %d read: %v\n", n, err)
			errCount++
			if errCount >= uint64(*maxErrors) {
				return 1
			}
			continue
		}
		if _, err := cdb.ApplyBlock(raw, n, opts, &stats); err != nil {
			fmt.Fprintf(os.Stderr, "block %d apply: %v\n", n, err)
			errCount++
			if errCount >= uint64(*maxErrors) {
				return 1
			}
			continue
		}
		applied++
		if uint64(n)%uint64(*progressEvery) == 0 {
			elapsed := time.Since(start)
			rate := float64(applied) / elapsed.Seconds()
			fmt.Printf("  block %d ok — %.0f blocks/s, %d applied, %d ops_applied, %d ops_skipped\n",
				n, rate, applied, stats.OpsApplied, stats.OpsSkipped)
		}
	}

	dur := time.Since(start)
	head, _ := cdb.HeadBlock()
	dgpo, _ := objects.GetDGPO(state.NewSession(stateDB))

	fmt.Println()
	fmt.Println("--- replay summary ---")
	fmt.Printf("blocks applied: %d (full=%d dirty=%d)\n", applied, stats.BlocksFull, stats.BlocksDirty)
	fmt.Printf("ops applied:    %d\n", stats.OpsApplied)
	fmt.Printf("ops skipped:    %d\n", stats.OpsSkipped)
	fmt.Printf("block errors:   %d\n", errCount)
	fmt.Printf("elapsed:        %s\n", dur.Round(time.Millisecond))
	if applied > 0 {
		fmt.Printf("rate:           %.0f blocks/s\n", float64(applied)/dur.Seconds())
	}
	fmt.Printf("head block:     %d\n", head)
	fmt.Printf("dgpo time:      %d\n", dgpo.Time)
	fmt.Printf("dgpo witness:   %s\n", dgpo.CurrentWitness)
	if errCount == 0 && stats.OpsSkipped == 0 {
		fmt.Println("status:         PASS — every op applied cleanly")
		return 0
	}
	if errCount == 0 {
		fmt.Println("status:         PARTIAL — pipeline drove all blocks; some ops skipped (expected with stubs)")
		// Print top skip reasons so the operator sees which evaluators
		// are responsible for the missing-balance cascades.
		if len(stats.SkipReasons) > 0 {
			type kv struct {
				key string
				val uint64
			}
			items := make([]kv, 0, len(stats.SkipReasons))
			for k, v := range stats.SkipReasons {
				items = append(items, kv{k, v})
			}
			sort.Slice(items, func(i, j int) bool { return items[i].val > items[j].val })
			fmt.Println("top skip reasons:")
			top := 15
			if len(items) < top {
				top = len(items)
			}
			for _, it := range items[:top] {
				fmt.Printf("  %8d  %s\n", it.val, it.key)
			}
		}
		return 0
	}
	return 1
}
