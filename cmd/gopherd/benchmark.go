package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"hived-go/internal/blocklog"
	"hived-go/internal/chain"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
)

// cmdBenchmark runs a cold-start replay benchmark and reports
// wall-clock + peak RSS + per-block rate. Output is a single
// machine-readable line + a human summary, matching what hived's
// `--replay-blockchain` benchmarks measure.
//
// Usage: ghive-tools benchmark --blocks=1000000 <block_log>
func cmdBenchmark(args []string) int {
	fs := flag.NewFlagSet("benchmark", flag.ContinueOnError)
	blocks := fs.Uint("blocks", 1_000_000, "number of blocks to replay")
	cacheMB := fs.Int("cache-mb", 512, "Pebble cache in MB")
	dataDir := fs.String("data-dir", "", "data directory (default: tempdir, deleted at end)")
	jsonOutput := fs.Bool("json", false, "emit a one-line JSON summary instead of human text")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: gopherd benchmark [flags] <block_log_path>")
		return 2
	}
	logPath := rest[0]

	dir := *dataDir
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "ghive-bench-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "benchmark: tempdir: %v\n", err)
			return 1
		}
		defer os.RemoveAll(dir)
	}

	stateDB, err := state.Open(dir, int64(*cacheMB)<<20)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchmark: open state: %v\n", err)
		return 1
	}
	defer stateDB.Close()

	if err := genesis.Apply(stateDB); err != nil {
		fmt.Fprintf(os.Stderr, "benchmark: genesis: %v\n", err)
		return 1
	}

	bl, err := blocklog.Open(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchmark: open block_log: %v\n", err)
		return 1
	}
	defer bl.Close()

	end := uint32(*blocks)
	if end > bl.Head() {
		end = bl.Head()
	}

	cdb := chain.New(stateDB)
	opts := chain.ApplyOptions{
		Sync:                      false,
		SkipUnknownOps:            false,
		TolerateFailingOps:        true,
		SkipTransactionSignatures: true,
		SkipBlockSignature:        true,
	}

	var memStart, memEnd, memPeak runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memStart)
	stats := chain.Stats{}

	start := time.Now()
	for n := uint32(1); n <= end; n++ {
		raw, err := bl.Block(n)
		if err != nil {
			fmt.Fprintf(os.Stderr, "block %d read: %v\n", n, err)
			return 1
		}
		if _, err := cdb.ApplyBlock(raw, n, opts, &stats); err != nil {
			fmt.Fprintf(os.Stderr, "block %d apply: %v\n", n, err)
			return 1
		}
		// Sample peak RSS every ~10k blocks (cheap; no IO).
		if n%10000 == 0 {
			runtime.ReadMemStats(&memEnd)
			if memEnd.HeapInuse > memPeak.HeapInuse {
				memPeak = memEnd
			}
		}
	}
	dur := time.Since(start)
	runtime.GC()
	runtime.ReadMemStats(&memEnd)
	if memEnd.HeapInuse > memPeak.HeapInuse {
		memPeak = memEnd
	}

	rate := float64(end) / dur.Seconds()
	if *jsonOutput {
		fmt.Printf(`{"blocks":%d,"elapsed_sec":%.3f,"rate_blocks_per_sec":%.1f,`+
			`"ops_applied":%d,"ops_skipped":%d,"heap_peak_mb":%d,"heap_end_mb":%d,`+
			`"sys_mb":%d,"go_version":%q,"goroutines":%d}`+"\n",
			end, dur.Seconds(), rate,
			stats.OpsApplied, stats.OpsSkipped,
			memPeak.HeapInuse>>20, memEnd.HeapInuse>>20,
			memEnd.Sys>>20, runtime.Version(), runtime.NumGoroutine())
		return 0
	}
	fmt.Println("--- ghive replay benchmark ---")
	fmt.Printf("blocks:        %d (head=%d)\n", end, bl.Head())
	fmt.Printf("wall-clock:    %s\n", dur)
	fmt.Printf("rate:          %.0f blocks/sec\n", rate)
	fmt.Printf("ops applied:   %d\n", stats.OpsApplied)
	fmt.Printf("ops skipped:   %d (%.4f%%)\n", stats.OpsSkipped,
		100*float64(stats.OpsSkipped)/float64(stats.OpsApplied+stats.OpsSkipped))
	fmt.Printf("heap peak:     %d MB\n", memPeak.HeapInuse>>20)
	fmt.Printf("heap end:      %d MB\n", memEnd.HeapInuse>>20)
	fmt.Printf("sys:           %d MB\n", memEnd.Sys>>20)
	fmt.Printf("Go:            %s\n", runtime.Version())
	fmt.Printf("goroutines:    %d\n", runtime.NumGoroutine())
	return 0
}
