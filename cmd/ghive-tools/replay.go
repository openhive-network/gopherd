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
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"time"

	"hived-go/internal/blocklog"
	"hived-go/internal/chain"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
	"hived-go/internal/state/snapshot"
)

func cmdReplay(args []string) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "Pebble data dir (defaults to a tempdir; pass a real path to persist across runs)")
	startBlock := fs.Uint("start", 1, "first block to apply")
	count := fs.Uint("count", 0, "number of blocks to apply (0 = all up to head)")
	progressEvery := fs.Uint("progress-every", 10_000, "print progress every N blocks")
	maxErrors := fs.Uint("max-errors", 5, "abort after this many apply errors")
	tolerateFailing := fs.Bool("tolerate-failing-ops", false, "skip ops whose evaluator returns an error (use during P1 incremental development; never on a real node)")
	watchAccount := fs.String("watch-account", "", "if set, print every block where the named account's balance/HBD/vesting changes")
	pprofAddr := fs.String("pprof", "", "if set, expose net/http/pprof on this addr (e.g. localhost:6060)")
	cpuProfile := fs.String("cpuprofile", "", "if set, write CPU profile to this file (collected while replay is running)")
	memProfile := fs.String("memprofile", "", "if set, write heap memory profile to this file at end of replay")
	cacheMB := fs.Int("cache-mb", 256, "Pebble block cache size in MiB (ignored for --backend=mem)")
	backend := fs.String("backend", "pebble", "state backend: pebble (persistent) or mem (in-memory btree, faster)")
	residentCap := fs.Int("comment-resident-cap", 0, "if >0, cap RAM-resident comments to this many; frozen comments (and their votes) spill to an on-disk scratch cold tier. Breaks the RAM ceiling for long replays (e.g. 4000000 keeps the cashout window hot).")
	coldDir := fs.String("cold-dir", "", "directory for the scratch cold tier (default: a tempdir, removed on exit). Only used when --comment-resident-cap>0.")
	evictBudget := fs.Int("evict-budget", 20000, "max comments evicted per block when over the resident cap")
	loadSnapshot := fs.String("load-snapshot", "", "if set, load state from this snapshot path (file = legacy single-file format; directory = block-N dir produced by --snapshot-every; skips genesis + replay before snapshot block)")
	saveSnapshotAt := fs.Uint("save-snapshot-at", 0, "block number after which to save a single-file snapshot (0 = don't save)")
	saveSnapshotTo := fs.String("save-snapshot-to", "", "single-file snapshot output path (used with --save-snapshot-at)")
	saveDirSnapshotAt := fs.Uint("save-dir-snapshot-at", 0, "block number after which to save a directory snapshot (covers typed stores too; 0 = don't save)")
	saveDirSnapshotTo := fs.String("save-dir-snapshot-to", "", "directory snapshot root (block-N/ subdir is created; used with --save-dir-snapshot-at)")
	snapshotEvery := fs.Uint("snapshot-every", 0, "save a directory snapshot every N applied blocks (0 = off)")
	snapshotDir := fs.String("snapshot-dir", "", "directory under which periodic snapshots are saved (block-<N>/ subdir per snapshot, used with --snapshot-every)")
	checkInvariants := fs.Uint("check-invariants", 0, "verify HIVE/HBD/VESTS conservation laws every N blocks (0 = off); aborts at the first block where the books don't balance — catches silent drift WITHOUT a reference hived")
	checkInvariantsStop := fs.Bool("check-invariants-stop", true, "stop replay at the first invariant violation (set false to log and continue)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *pprofAddr != "" {
		go func() {
			fmt.Fprintf(os.Stderr, "replay: pprof listening on http://%s/debug/pprof/\n", *pprofAddr)
			if err := http.ListenAndServe(*pprofAddr, nil); err != nil {
				fmt.Fprintf(os.Stderr, "replay: pprof server: %v\n", err)
			}
		}()
	}
	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "replay: cpu profile: %v\n", err)
			return 1
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			fmt.Fprintf(os.Stderr, "replay: start cpu profile: %v\n", err)
			return 1
		}
		defer pprof.StopCPUProfile()
		fmt.Fprintf(os.Stderr, "replay: cpu profile -> %s\n", *cpuProfile)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: ghive-tools replay [flags] <block_log_path>")
		return 2
	}
	logPath := rest[0]

	var stateDB *state.Database
	switch *backend {
	case "mem":
		stateDB = state.OpenMem()
		fmt.Println("replay: using in-memory btree backend (state is volatile)")
	case "pebble", "":
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
		cacheBytes := int64(*cacheMB) << 20
		var err error
		stateDB, err = state.Open(dir, cacheBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "replay: open state: %v\n", err)
			return 1
		}
	default:
		fmt.Fprintf(os.Stderr, "replay: unknown backend %q (want pebble or mem)\n", *backend)
		return 2
	}
	defer stateDB.Close()

	// Pre-register every typed subsystem on the freshly-opened
	// Database so snapshot saves include a uniform set regardless of
	// whether the chain has yet exercised each store. Without this,
	// a snapshot at block 2000 (no comments yet) would lack
	// comment_store, while a snapshot at block 2000 produced AFTER
	// loading an earlier snapshot would include it (because the
	// loader pre-registers all stores) — non-deterministic snapshot
	// content that breaks bisection.
	{
		boot := state.NewSession(stateDB)
		objects.EnsureWitnessStore(boot)
		objects.EnsureWitnessVoteStore(boot)
		objects.EnsureAccountStore(boot)
		objects.EnsureCommentStore(boot)
		objects.EnsureCommentVoteStore(boot)
		objects.EnsureBlockSummaryStore(boot)
		objects.EnsureWitnessScheduleStore(boot)
		objects.EnsureFutureWitnessScheduleStore(boot)
		objects.EnsureDGPOStore(boot)
		objects.EnsureCommentCashoutScheduleStore(boot)
		objects.EnsureLimitOrderStore(boot)
		objects.EnsureVestingWithdrawalScheduleStore(boot)
		objects.EnsureTransactionStore(boot)
		objects.EnsureConvertRequestStore(boot)
	}

	if *loadSnapshot != "" {
		var head uint32
		st, sterr := os.Stat(*loadSnapshot)
		if sterr != nil {
			fmt.Fprintf(os.Stderr, "replay: stat snapshot: %v\n", sterr)
			return 1
		}
		if st.IsDir() {
			man, err := snapshot.Load(stateDB, *loadSnapshot)
			if err != nil {
				fmt.Fprintf(os.Stderr, "replay: load snapshot dir: %v\n", err)
				return 1
			}
			head = man.BlockNum
		} else {
			h, err := stateDB.LoadSnapshot(*loadSnapshot)
			if err != nil {
				fmt.Fprintf(os.Stderr, "replay: load snapshot: %v\n", err)
				return 1
			}
			head = h
		}
		fmt.Printf("replay: loaded snapshot %s at block %d\n", *loadSnapshot, head)
		// If --start was left at the default 1, advance to head+1 so the
		// loop picks up where the snapshot left off.
		if *startBlock == 1 {
			*startBlock = uint(head) + 1
		}
	} else {
		if err := genesis.Apply(stateDB); err != nil {
			fmt.Fprintf(os.Stderr, "replay: genesis: %v\n", err)
			return 1
		}
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

	// Windowed fine-grained invariant checking (see the check site below).
	var invFineFrom, invFineEvery uint64
	if v := os.Getenv("GHIVE_INV_FINE_FROM"); v != "" {
		invFineFrom, _ = strconv.ParseUint(v, 10, 64)
	}
	if v := os.Getenv("GHIVE_INV_FINE_EVERY"); v != "" {
		invFineEvery, _ = strconv.ParseUint(v, 10, 64)
	}
	if invFineFrom > 0 {
		fmt.Printf("replay: fine invariant checks every %d blocks from block %d\n", invFineEvery, invFineFrom)
	}

	// Cold tier: spill frozen comments + their votes to an on-disk scratch
	// store so the RAM working set stays bounded on long replays. Attached
	// AFTER any snapshot load (the store objects persist across sessions, so
	// the attachment survives). Disabled when --comment-resident-cap=0.
	var commentStoreForEvict *objects.CommentStore
	var voteStoreForEvict *objects.CommentVoteStore
	if *residentCap > 0 {
		cold, cerr := objects.OpenColdKV(*coldDir)
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "replay: open cold tier: %v\n", cerr)
			return 1
		}
		defer cold.Close()
		boot := state.NewSession(stateDB)
		commentStoreForEvict = objects.EnsureCommentStore(boot)
		voteStoreForEvict = objects.EnsureCommentVoteStore(boot)
		commentStoreForEvict.SetColdTier(cold)
		commentStoreForEvict.SetResidentCap(*residentCap)
		voteStoreForEvict.SetColdTier(cold)
		fmt.Printf("replay: cold tier ON — resident cap %d comments, evict budget %d/block, dir %s\n",
			*residentCap, *evictBudget, cold.Dir())
	}

	cdb := chain.New(stateDB)
	start := time.Now()
	var (
		applied, errCount uint64
		stats             chain.Stats
	)
	opts := chain.ApplyOptions{
		Sync:                      false,
		SkipUnknownOps:            false,
		TolerateFailingOps:        *tolerateFailing,
		SkipTransactionSignatures: true, // replay always skips sigs
		SkipBlockSignature:        true,
		SkipDedupAndTaPOS:         true, // canonical block_log; sha256+dedup+TaPOS+prune redundant
		SkipTxSavepoints:          true, // canonical block_log; per-tx undo snap captures redundant
	}

	type acctSnap struct {
		balance, hbd, vesting, savings, savingsHBD int64
		exists                                     bool
	}
	prevSnap := acctSnap{}
	snapshotAccount := func(name string) acctSnap {
		s := state.NewSession(stateDB)
		a, err := objects.GetAccount(s, name)
		if err != nil {
			return acctSnap{}
		}
		return acctSnap{
			balance:    a.Balance,
			hbd:        a.HBDBalance,
			vesting:    a.VestingShares,
			savings:    a.SavingsBalance,
			savingsHBD: a.SavingsHBDBalance,
			exists:     true,
		}
	}

	var rawScratch, outScratch []byte
	for n := uint32(*startBlock); n <= end; n++ {
		raw, newRaw, err := bl.BlockReuse(n, rawScratch, outScratch)
		rawScratch = newRaw
		// raw aliases outScratch (or a fresh alloc when block was bigger).
		// Capture it as the next-iteration outScratch reuse buffer; the
		// current iteration must not retain `raw` past the next loop step.
		outScratch = raw
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
		// Post-commit safe point: spill frozen comments + votes to the cold
		// tier if over the resident cap (no-op when the cold tier is off).
		if commentStoreForEvict != nil {
			commentStoreForEvict.EvictFrozen(voteStoreForEvict, *evictBudget)
		}
		if *watchAccount != "" {
			cur := snapshotAccount(*watchAccount)
			if cur != prevSnap {
				fmt.Printf("  WATCH %s @block %d: bal %d→%d hbd %d→%d vests %d→%d savings %d→%d savings_hbd %d→%d\n",
					*watchAccount, n,
					prevSnap.balance, cur.balance,
					prevSnap.hbd, cur.hbd,
					prevSnap.vesting, cur.vesting,
					prevSnap.savings, cur.savings,
					prevSnap.savingsHBD, cur.savingsHBD)
				prevSnap = cur
			}
		}
		invEvery := uint64(*checkInvariants)
		// GHIVE_INV_FINE_FROM / GHIVE_INV_FINE_EVERY: past the FROM block,
		// check invariants every EVERY blocks instead of the coarse cadence,
		// so one full replay can pinpoint a conservation-break onset window
		// without a (memory-heavy) warm-start.
		if invFineFrom > 0 && uint64(n) >= invFineFrom && invFineEvery > 0 {
			invEvery = invFineEvery
		}
		if invEvery != 0 && uint64(n)%invEvery == 0 {
			chkSess := state.NewSession(stateDB)
			r, cerr := chain.CheckInvariants(chkSess)
			if cerr != nil {
				fmt.Fprintf(os.Stderr, "block %d invariants: %v\n", n, cerr)
			} else if !r.OK() {
				fmt.Fprintf(os.Stderr, "INVARIANT FAIL block=%d accounts=%d%s\n", n, r.Accounts, r.FailString())
				if *checkInvariantsStop {
					fmt.Printf("replay: aborting at first invariant violation (block %d)\n", n)
					return 1
				}
			}
		}
		if uint64(n)%uint64(*progressEvery) == 0 {
			elapsed := time.Since(start)
			rate := float64(applied) / elapsed.Seconds()
			fmt.Printf("  block %d ok — %.0f blocks/s, %d applied, %d ops_applied, %d ops_skipped\n",
				n, rate, applied, stats.OpsApplied, stats.OpsSkipped)
		}
		if *saveSnapshotAt != 0 && n == uint32(*saveSnapshotAt) && *saveSnapshotTo != "" {
			if err := stateDB.SaveSnapshot(*saveSnapshotTo, n); err != nil {
				fmt.Fprintf(os.Stderr, "replay: save snapshot: %v\n", err)
				return 1
			}
			fmt.Printf("replay: saved snapshot to %s at block %d\n", *saveSnapshotTo, n)
		}
		if *saveDirSnapshotAt != 0 && n == uint32(*saveDirSnapshotAt) && *saveDirSnapshotTo != "" {
			path, err := snapshot.Save(stateDB, *saveDirSnapshotTo, n, "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "replay: save dir snapshot: %v\n", err)
				return 1
			}
			fmt.Printf("replay: saved dir snapshot %s at block %d\n", path, n)
		}
		if *snapshotEvery != 0 && *snapshotDir != "" && uint64(n)%uint64(*snapshotEvery) == 0 {
			snapStart := time.Now()
			path, err := snapshot.Save(stateDB, *snapshotDir, n, "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "replay: save snapshot dir: %v\n", err)
				return 1
			}
			fmt.Printf("replay: saved snapshot %s (%.1fs)\n", path, time.Since(snapStart).Seconds())
		}
	}

	dur := time.Since(start)
	if *memProfile != "" {
		f, err := os.Create(*memProfile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "replay: mem profile: %v\n", err)
		} else {
			runtime.GC()
			if err := pprof.WriteHeapProfile(f); err != nil {
				fmt.Fprintf(os.Stderr, "replay: write heap profile: %v\n", err)
			}
			f.Close()
			fmt.Fprintf(os.Stderr, "replay: mem profile -> %s\n", *memProfile)
		}
	}
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
		if len(stats.SkipAccounts) > 0 {
			type kv struct {
				key string
				val uint64
			}
			items := make([]kv, 0, len(stats.SkipAccounts))
			for k, v := range stats.SkipAccounts {
				items = append(items, kv{k, v})
			}
			sort.Slice(items, func(i, j int) bool { return items[i].val > items[j].val })
			fmt.Println("top starved accounts:")
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
