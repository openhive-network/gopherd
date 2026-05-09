// Command drift-hunter scans every interesting field of ghive's state
// and hived's state at a given block (or set of blocks) and reports the
// FIRST diverging fields across the entire surface.
//
// Workflow assumed:
//
//   1. Drive a Go-side replay or save snapshot:
//        ./bin/ghive-tools replay --save-snapshot-to=./snapshots/blk-N --stop-at=N
//      (or use --go-replay=PATH/block_log to replay inside the tool).
//
//   2. Spin up hived API at the same block (a separate agent's wrapper
//      around scripts/hived-prepare-checkpoint.sh + hived-serve-checkpoint.sh):
//        scripts/hived-prepare-checkpoint.sh N
//        scripts/hived-serve-checkpoint.sh   N &
//      Then hived listens on http://localhost:18090 by default.
//
//   3. Run drift-hunter:
//        ./bin/drift-hunter --block=N --hived-url=http://localhost:18090 \
//                           --go-snapshot=./snapshots/blk-N
//
// Output format is one drift per line, greppable:
//
//   BLOCK 5,148,656: dgpo.current_aslot Go=5193905 hived=5194004 (Δ=-99)
//   BLOCK 5,148,656: witness_schedule.shuffled[03] Go="rabbit-13" hived="rabbit-18"
//   BLOCK 5,148,656: witness:gxt-1080-sc-0028.signing_key.hex_or_b58_marker ...
//
// Range mode (--range=START:STOP:STRIDE) repeats the scan at every
// stride'th block in [START, STOP] and reports the FIRST block where
// any drift appears, along with which fields diverged.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state/objects"
)

// runFlags bundles every command-line option. Keeping them in a struct
// makes it cheap to pass into the per-block runner.
type runFlags struct {
	hivedURL    string
	block       uint32
	rangeSpec   string
	goSnapshot  string
	goReplay    string
	witnesses   string // comma-separated; "" = top-21 by votes
	topN        int
	skipWitness bool
	accounts    string // comma-separated account names to compare
}

func parseFlags() *runFlags {
	f := &runFlags{}
	flag.StringVar(&f.hivedURL, "hived-url", "http://localhost:18090", "hived JSON-RPC endpoint")
	blockArg := flag.Uint("block", 0, "single target block (mutually exclusive with --range)")
	flag.StringVar(&f.rangeSpec, "range", "", "scan a range as START:STOP:STRIDE (e.g. 5000000:5200000:100000)")
	flag.StringVar(&f.goSnapshot, "go-snapshot", "", "path to a Go state snapshot (preferred)")
	flag.StringVar(&f.goReplay, "go-replay", "", "path to block_log; replay from genesis (slow fallback)")
	flag.StringVar(&f.witnesses, "witness", "", "comma-separated witness names to compare (default: top-N by votes)")
	flag.IntVar(&f.topN, "top-witnesses", 21, "compare the top-N witnesses by votes")
	flag.BoolVar(&f.skipWitness, "skip-witness", false, "skip per-witness comparators (DGPO/schedule/feed/funds only)")
	flag.StringVar(&f.accounts, "account", "", "comma-separated account names to compare (balances + vesting)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: drift-hunter [flags]")
		fmt.Fprintln(os.Stderr, "  --block=N                 single block")
		fmt.Fprintln(os.Stderr, "  --range=A:B:S             scan blocks [A..B] stride S")
		fmt.Fprintln(os.Stderr, "  --hived-url=URL           hived JSON-RPC (default localhost:18090)")
		fmt.Fprintln(os.Stderr, "  --go-snapshot=PATH        Go state snapshot to load (preferred)")
		fmt.Fprintln(os.Stderr, "  --go-replay=BLOCKLOG      block_log to replay (slow fallback)")
		fmt.Fprintln(os.Stderr, "  --top-witnesses=N         compare top-N witnesses by votes (default 21)")
		fmt.Fprintln(os.Stderr, "  --witness=A,B,C           compare specific witnesses by name")
		fmt.Fprintln(os.Stderr, "  --skip-witness            skip per-witness comparators entirely")
		fmt.Fprintln(os.Stderr, "  --account=A,B,C           compare named accounts (balances + vesting)")
	}
	flag.Parse()
	f.block = uint32(*blockArg)
	return f
}

// runOne executes every comparator at one block and returns the
// aggregated drift list. If hived is unreachable the caller should
// have already checked via probe(); otherwise this returns drifts that
// surface the failure.
func runOne(f *runFlags, rpc *rpcClient, block uint32) ([]drift, error) {
	// 1. Get Go state.
	var (
		g   *goState
		err error
	)
	switch {
	case f.goSnapshot != "":
		g, err = loadGoSnapshot(f.goSnapshot)
	case f.goReplay != "":
		g, err = replayGoToBlock(f.goReplay, block)
	default:
		return nil, fmt.Errorf("must pass --go-snapshot or --go-replay")
	}
	if err != nil {
		return nil, fmt.Errorf("go state at block %d: %w", block, err)
	}
	// Sanity-check: if the snapshot's recorded head doesn't match the
	// requested block, surface as a single drift (so the user knows
	// they passed a wrong snapshot) but continue — the comparators
	// might still produce useful info.
	if g.blockNum != block && f.goSnapshot != "" {
		fmt.Fprintf(os.Stderr, "WARN snapshot head=%d, requested block=%d (using snapshot's head)\n",
			g.blockNum, block)
	}

	// 2. Pull hived state.
	hDGPO, err := rpc.fetchDGPO()
	if err != nil {
		return nil, fmt.Errorf("hived dgpo: %w", err)
	}
	if hDGPO.HeadBlockNumber != block {
		fmt.Fprintf(os.Stderr, "WARN hived head=%d, requested block=%d\n",
			hDGPO.HeadBlockNumber, block)
	}

	hSched, schedErr := rpc.fetchWitnessSchedule()
	hFeed, feedErr := rpc.fetchFeedHistory()
	hFunds, fundsErr := rpc.fetchRewardFunds()

	// 3. Run comparators.
	var allDrifts []drift
	allDrifts = append(allDrifts, compareDGPO(block, g, hDGPO)...)
	if schedErr == nil {
		allDrifts = append(allDrifts, compareWitnessSchedule(block, g, hSched)...)
	} else {
		allDrifts = append(allDrifts, drift{
			block: block, table: "witness_schedule", field: "_fetch_err",
			go_: "(skipped)", hived: schedErr.Error(),
		})
	}
	if feedErr == nil {
		allDrifts = append(allDrifts, compareFeedHistory(block, g, hFeed)...)
	} else {
		allDrifts = append(allDrifts, drift{
			block: block, table: "feed_history", field: "_fetch_err",
			go_: "(skipped)", hived: feedErr.Error(),
		})
	}
	if fundsErr == nil {
		allDrifts = append(allDrifts, compareRewardFunds(block, g, hFunds)...)
	} else {
		allDrifts = append(allDrifts, drift{
			block: block, table: "reward_fund:?", field: "_fetch_err",
			go_: "(skipped)", hived: fundsErr.Error(),
		})
	}

	// 4. Per-witness comparators.
	if !f.skipWitness {
		witnessNames := strings.Split(f.witnesses, ",")
		if f.witnesses == "" {
			// Default: top-N by votes from hived.
			tops, err := rpc.fetchTopWitnessesByVotes(f.topN)
			if err != nil {
				allDrifts = append(allDrifts, drift{
					block: block, table: "witness:?", field: "_fetch_err",
					go_: "(skipped)", hived: err.Error(),
				})
			} else {
				witnessNames = make([]string, 0, len(tops))
				for _, w := range tops {
					witnessNames = append(witnessNames, w.Owner)
				}
			}
		}
		// Index Go-side witnesses by name once.
		gws, _ := g.witnesses()
		goByName := make(map[string]int, len(gws))
		for i, w := range gws {
			goByName[string(w.Owner)] = i
		}

		for _, name := range witnessNames {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			hw, err := rpc.fetchWitnessByName(name)
			if err != nil {
				allDrifts = append(allDrifts, drift{
					block: block, table: "witness:" + name, field: "_fetch_err",
					go_: "(skipped)", hived: err.Error(),
				})
				continue
			}
			gi, ok := goByName[name]
			if !ok {
				// Witness exists in hived, missing in Go.
				allDrifts = append(allDrifts, drift{
					block: block, table: "witness:" + name, field: "_present",
					go_: "absent", hived: "present",
				})
				continue
			}
			gw := gws[gi]
			// Sanity: the AccountName type is a string, so the comparison
			// in compareWitness uses a value-copy of gw.
			gw.Owner = auth.AccountName(name)
			allDrifts = append(allDrifts, compareWitness(block, gw, hw)...)
		}
	}

	// 5. Per-account comparators (--account=A,B,C). Used for chasing
	// balance-divergence bugs where a specific account is short.
	if f.accounts != "" {
		for _, name := range strings.Split(f.accounts, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			ha, err := rpc.fetchAccountByName(name)
			if err != nil {
				allDrifts = append(allDrifts, drift{
					block: block, table: "account:" + name, field: "_fetch_err",
					go_: "(skipped)", hived: err.Error(),
				})
				continue
			}
			ga, gerr := objects.GetAccount(g.session, auth.AccountName(name))
			if gerr != nil {
				allDrifts = append(allDrifts, drift{
					block: block, table: "account:" + name, field: "_present",
					go_: "absent", hived: "present",
				})
				continue
			}
			ga.Name = auth.AccountName(name)
			allDrifts = append(allDrifts, compareAccount(block, ga, ha)...)
		}
	}

	sortDrifts(allDrifts)
	return allDrifts, nil
}

// runRange executes runOne at every stride'th block in [start..stop]
// and prints, for each one, either "no drift" or the full drift list.
// Returns the FIRST block where any drift appeared (0 if none).
//
// Caveat: this does NOT spin up hived for each block — the user is
// expected to have hived serving the right block, OR to feed in a
// pre-existing snapshot path that varies per block. For now we print a
// warning when block != hived.head_block_number.
func runRange(f *runFlags, rpc *rpcClient, start, stop, stride uint32) (uint32, error) {
	if stride == 0 {
		stride = 1
	}
	first := uint32(0)
	for b := start; b <= stop; b += stride {
		fmt.Printf("=== scanning block %s ===\n", humanBlock(b))
		drifts, err := runOne(f, rpc, b)
		if err != nil {
			fmt.Fprintf(os.Stderr, "block %d: %v\n", b, err)
			continue
		}
		if len(drifts) == 0 {
			fmt.Printf("BLOCK %s: no drift\n", humanBlock(b))
			continue
		}
		if first == 0 {
			first = b
		}
		for _, d := range drifts {
			fmt.Println(d.String())
		}
	}
	if first != 0 {
		fmt.Printf("=== first drift at block %s ===\n", humanBlock(first))
	} else {
		fmt.Println("=== no drift across scanned range ===")
	}
	return first, nil
}

// parseRange decodes "A:B:S" into three uint32s. Returns an error if
// the format is wrong.
func parseRange(spec string) (start, stop, stride uint32, err error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("--range must be START:STOP:STRIDE")
	}
	a, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("range start: %w", err)
	}
	b, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("range stop: %w", err)
	}
	s, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("range stride: %w", err)
	}
	if a > b {
		return 0, 0, 0, fmt.Errorf("range start > stop")
	}
	return uint32(a), uint32(b), uint32(s), nil
}

// summarize produces a one-block summary breakdown like
//
//   block X: 12 drifts (6 dgpo, 4 schedule, 2 witness:bob)
//
// Useful when piping through head + grep.
func summarize(block uint32, drifts []drift) {
	if len(drifts) == 0 {
		fmt.Printf("BLOCK %s: no drift across all comparators\n", humanBlock(block))
		return
	}
	counts := make(map[string]int, 8)
	for _, d := range drifts {
		counts[d.table]++
	}
	tables := make([]string, 0, len(counts))
	for t := range counts {
		tables = append(tables, t)
	}
	sort.Slice(tables, func(i, j int) bool {
		if r := tableRank(tables[i]) - tableRank(tables[j]); r != 0 {
			return r < 0
		}
		return tables[i] < tables[j]
	})
	parts := make([]string, 0, len(tables))
	for _, t := range tables {
		parts = append(parts, fmt.Sprintf("%d %s", counts[t], t))
	}
	fmt.Fprintf(os.Stderr, "BLOCK %s: %d drifts (%s)\n",
		humanBlock(block), len(drifts), strings.Join(parts, ", "))
}

func main() {
	f := parseFlags()
	if f.block == 0 && f.rangeSpec == "" {
		fmt.Fprintln(os.Stderr, "drift-hunter: must pass --block=N or --range=A:B:S")
		os.Exit(2)
	}
	if f.goSnapshot == "" && f.goReplay == "" {
		fmt.Fprintln(os.Stderr, "drift-hunter: must pass --go-snapshot=PATH or --go-replay=BLOCKLOG")
		os.Exit(2)
	}

	rpc := newRPCClient(f.hivedURL)
	if err := rpc.probe(); err != nil {
		fmt.Fprintf(os.Stderr, "drift-hunter: hived API at %s not responding: %v\n", f.hivedURL, err)
		fmt.Fprintln(os.Stderr, "Did you start scripts/hived-serve-checkpoint.sh <block>?")
		os.Exit(1)
	}

	if f.rangeSpec != "" {
		start, stop, stride, err := parseRange(f.rangeSpec)
		if err != nil {
			fmt.Fprintln(os.Stderr, "drift-hunter:", err)
			os.Exit(2)
		}
		first, err := runRange(f, rpc, start, stop, stride)
		if err != nil {
			fmt.Fprintln(os.Stderr, "drift-hunter:", err)
			os.Exit(1)
		}
		if first != 0 {
			os.Exit(0) // exit 0 even when drifts are found — this is a debug tool
		}
		return
	}

	drifts, err := runOne(f, rpc, f.block)
	if err != nil {
		fmt.Fprintln(os.Stderr, "drift-hunter:", err)
		os.Exit(1)
	}
	for _, d := range drifts {
		fmt.Println(d.String())
	}
	summarize(f.block, drifts)
}
