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
	"gopherd/internal/config"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"gopherd/internal/blocklog"
	"gopherd/internal/chain"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
	"gopherd/internal/state/snapshot"
)

// snapBlockNum parses the block height from a snapshot dir path of the form
// ".../block-<N>" (used to pick the HF era before loading). Returns 0 if the
// path isn't in that form.
func snapBlockNum(path string) uint32 {
	base := filepath.Base(path)
	const pfx = "block-"
	if !strings.HasPrefix(base, pfx) {
		return 0
	}
	// Tolerate suffixed variants like "block-56000000-fixed" (repair copies):
	// parse the leading digit run only. A parse-to-0 here silently selects
	// the pre-HF17 by-root cold mode for the load spill (double cold writes).
	digits := base[len(pfx):]
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			digits = digits[:i]
			break
		}
	}
	n, err := strconv.ParseUint(digits, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}

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
	coldMmap := fs.Bool("cold-mmap", false, "use the mmap cold-tier backend (faster, zero-copy) instead of pebble")
	coldBackend := fs.String("cold-backend", "", "cold-tier backend: \"\"/mmap|pebble|persist. persist = chainbase-in-Go: append-only mmap + restored index, so the cold tier (the bulk of state) SURVIVES restart with no replay (requires a stable --cold-dir). Overrides --cold-mmap when set.")
	coldCompactRatio := fs.Float64("cold-compact-ratio", 0, "if >0 and using --cold-backend=persist, compact the cold tier (reclaim dead space) when its dead-space ratio exceeds this (e.g. 0.4); checked every --cold-compact-every blocks")
	coldCompactEvery := fs.Uint("cold-compact-every", 1000000, "block cadence for the --cold-compact-ratio dead-space check")
	shutdownSnapshotDir := fs.String("shutdown-snapshot-dir", "", "if set, on clean exit OR SIGTERM/SIGINT save a dir snapshot here (block-<head>/) and flush the cold tier — so a restart with --load-snapshot + the same --cold-dir resumes at head with ZERO replay tail")
	persistStateDir := fs.String("persist-state-dir", "", "ZERO-SNAPSHOT NODE: persist ALL non-cold state (accounts, singletons, witnesses, …) into a persiststore container here. With --cold-backend=persist + --cold-dir, the whole node state is two persiststore dirs: on clean exit/SIGTERM it's saved, and a restart with the SAME two dirs resumes at head — no snapshot file. Takes precedence over --load-snapshot/--shutdown-snapshot-dir.")
	evictBudget := fs.Int("evict-budget", 20000, "max comments evicted per block when over the resident cap")
	parallelApply := fs.Int("parallel-apply", 0, "if >0, apply each block's transactions via the optimistic parallel harness with this many workers (replay mode only; bit-identical to serial). 0 = serial (default).")
	loadSnapshot := fs.String("load-snapshot", "", "if set, load state from this snapshot path (file = legacy single-file format; directory = block-N dir produced by --snapshot-every; skips genesis + replay before snapshot block)")
	saveSnapshotAt := fs.Uint("save-snapshot-at", 0, "block number after which to save a single-file snapshot (0 = don't save)")
	saveSnapshotTo := fs.String("save-snapshot-to", "", "single-file snapshot output path (used with --save-snapshot-at)")
	saveDirSnapshotAt := fs.Uint("save-dir-snapshot-at", 0, "block number after which to save a directory snapshot (covers typed stores too; 0 = don't save)")
	saveDirSnapshotTo := fs.String("save-dir-snapshot-to", "", "directory snapshot root (block-N/ subdir is created; used with --save-dir-snapshot-at)")
	snapshotEvery := fs.Uint("snapshot-every", 0, "save a directory snapshot every N applied blocks (0 = off)")
	snapshotDir := fs.String("snapshot-dir", "", "directory under which periodic snapshots are saved (block-<N>/ subdir per snapshot, used with --snapshot-every)")
	checkInvariants := fs.Uint("check-invariants", 0, "verify HIVE/HBD/VESTS conservation laws every N blocks (0 = off); aborts at the first block where the books don't balance — catches silent drift WITHOUT a reference hived")
	checkInvariantsStop := fs.Bool("check-invariants-stop", true, "stop replay at the first invariant violation (set false to log and continue)")
	selfContainedSnap := fs.Bool("snapshot-self-contained", false, "when saving a snapshot on a persist cold backend, STREAM the full cold tier into the snapshot (portable, loads into any cold backend) instead of the default resident-only durable dump. Use for a frozen near-frontier checkpoint that each fix-test run reloads into a fresh volatile tier.")
	prefetch := fs.Int("prefetch", 8, "read+decompress this many blocks ahead of apply on a background goroutine (0 = serial read like before)")
	memLimitGiB := fs.Int("mem-limit-gib", 0, "if >0, set the Go runtime soft memory limit (debug.SetMemoryLimit) to this many GiB — same effect as GOMEMLIMIT but wired in-process so GC pacing always cooperates with the cgroup cap")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *memLimitGiB > 0 {
		debug.SetMemoryLimit(int64(*memLimitGiB) << 30)
		fmt.Fprintf(os.Stderr, "replay: Go memory limit set to %d GiB\n", *memLimitGiB)
	}
	if *selfContainedSnap {
		objects.SnapshotForceSelfContained = true
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
		fmt.Fprintln(os.Stderr, "usage: gopherd replay [flags] <block_log_path>")
		return 2
	}
	logPath := rest[0]

	// Graceful-shutdown flag. The signal handler is installed LATER, right before
	// the apply loop — NOT here — so that a SIGTERM/SIGINT during the (long,
	// uninterruptible) snapshot-load/genesis phase keeps the DEFAULT behavior
	// (terminate immediately), rather than being swallowed by a handler the load
	// loop never polls. There is nothing to flush mid-load, so immediate exit is
	// correct there.
	var stopRequested atomic.Bool

	// Replay is never "in control" (hived is_in_control()==false): RC
	// over-delegation clipping decisions depend on hived's full RC model
	// and misfire under our partial one — the delegate_rc op stream keeps
	// delegation state convergent instead. See objects.RCClipEnabled.
	objects.RCClipEnabled = false

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

	// Cold tier: spill frozen comments + their votes to an on-disk scratch
	// store so the RAM working set stays bounded. Attached BEFORE any snapshot
	// load so CommentStore.UnmarshalSnapshot can spill already-paid comments
	// straight to disk instead of loading every comment resident (a deep
	// snapshot has ~175M comments and would OOM a 31 GB box otherwise).
	// Disabled when --comment-resident-cap=0.
	var commentStoreForEvict *objects.CommentStore
	var voteStoreForEvict *objects.CommentVoteStore
	var cold objects.ColdStore
	if *residentCap > 0 {
		backendName := *coldBackend
		if backendName == "" && *coldMmap {
			backendName = "mmap" // back-compat: --cold-mmap selects the mmap backend
		}
		if backendName == "persist" && *coldDir == "" {
			fmt.Fprintln(os.Stderr, "replay: --cold-backend=persist requires a stable --cold-dir (a persistent cold tier can't live in a tempdir)")
			return 2
		}
		c, cerr := objects.OpenColdTierBackend(*coldDir, backendName)
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "replay: open cold tier: %v\n", cerr)
			return 1
		}
		cold = c
		defer cold.Close()
		boot := state.NewSession(stateDB)
		commentStoreForEvict = objects.EnsureCommentStore(boot)
		voteStoreForEvict = objects.EnsureCommentVoteStore(boot)
		commentStoreForEvict.SetColdTier(cold)
		commentStoreForEvict.SetResidentCap(*residentCap)
		voteStoreForEvict.SetColdTier(cold)
		// Post-HF17 the by-root cold index is never read (only the pre-HF17
		// cashout sibling-walk consumes it), so skip writing it — halves cold
		// writes (eviction + snapshot-load cost). Decide the era from the start
		// block: the snapshot block when resuming (set BEFORE load so the spill
		// honors it), else genesis. The per-block loop keeps it correct across
		// the HF17 boundary for genesis runs.
		startBlk := uint32(1)
		if *loadSnapshot != "" {
			if n := snapBlockNum(*loadSnapshot); n > 0 {
				startBlk = n
			}
		}
		if *persistStateDir != "" {
			if h, ok := snapshot.PersistStateHead(*persistStateDir); ok {
				startBlk = h
			}
		}
		byRootCold := !hardfork.SetForBlock(startBlk).Has(hardfork.HF17)
		commentStoreForEvict.SetColdByRootEnabled(byRootCold)
		fmt.Printf("replay: cold tier ON — resident cap %d comments, evict budget %d/block, by-root cold %v, dir %s\n",
			*residentCap, *evictBudget, byRootCold, cold.Dir())
	}

	persistResumed := false
	if *persistStateDir != "" {
		if _, ok := snapshot.PersistStateHead(*persistStateDir); ok {
			h, err := snapshot.LoadPersist(stateDB, *persistStateDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "replay: load persist-state: %v\n", err)
				return 1
			}
			// Zero-snapshot node: full state restored from the persiststore
			// container + the durable cold tier — no snapshot file involved.
			fmt.Printf("replay: resumed from persist-state %s at block %d (no snapshot file)\n", *persistStateDir, h)
			if *startBlock == 1 {
				*startBlock = uint(h) + 1
			}
			persistResumed = true
		}
	}
	if persistResumed {
		// state already restored from the persist-state container
	} else if *loadSnapshot != "" {
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
		if err := genesis.ApplyForNetwork(stateDB, nil); err != nil {
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
	if v := config.Env("GOPHERD_INV_FINE_FROM"); v != "" {
		invFineFrom, _ = strconv.ParseUint(v, 10, 64)
	}
	if v := config.Env("GOPHERD_INV_FINE_EVERY"); v != "" {
		invFineEvery, _ = strconv.ParseUint(v, 10, 64)
	}
	if invFineFrom > 0 {
		fmt.Printf("replay: fine invariant checks every %d blocks from block %d\n", invFineEvery, invFineFrom)
	}

	cdb := chain.New(stateDB)
	start := time.Now()
	var (
		applied, errCount uint64
		stats             chain.Stats
		headBlock         uint32 = uint32(*startBlock) - 1 // last fully-applied block
	)
	// Cold-tier compaction handle (only the persist backend supports reclaim).
	var compactCold objects.CompactableCold
	if cold != nil {
		compactCold, _ = cold.(objects.CompactableCold)
	}
	opts := chain.ApplyOptions{
		Sync:                      false,
		SkipUnknownOps:            false,
		ParallelApply:             *parallelApply,
		TolerateFailingOps:        *tolerateFailing,
		SkipTransactionSignatures: true, // replay always skips sigs
		SkipBlockSignature:        true,
		SkipDedupAndTaPOS:         true, // canonical block_log; sha256+dedup+TaPOS+prune redundant
		SkipTxSavepoints:          true, // canonical block_log; per-tx undo snap captures redundant
	}

	type acctSnap struct {
		balance, hbd, vesting, savings, savingsHBD int64
		rwdVests, rwdHive, rwdHBD                  int64
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
			rwdVests:   a.RewardVests,
			rwdHive:    a.RewardHive,
			rwdHBD:     a.RewardHBD,
			exists:     true,
		}
	}

	// Install the graceful-shutdown handler now that the load phase is done and
	// the apply loop (which polls stopRequested each block) is about to start.
	// First signal → clean stop at the next block boundary; a SECOND signal →
	// force exit (so a stuck shutdown is still killable without SIGKILL).
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigCh
		fmt.Fprintf(os.Stderr, "\nreplay: %v received — finishing current block then shutting down cleanly (signal again to force)...\n", s)
		stopRequested.Store(true)
		<-sigCh
		fmt.Fprintln(os.Stderr, "replay: second signal — forcing exit")
		os.Exit(130)
	}()

	// Block read+decompress pipeline: a single reader goroutine runs
	// bl.BlockReuse up to --prefetch blocks ahead of apply, recycling a fixed
	// ring of buffer pairs through channels. Overlaps the pread + zstd
	// decompress (which otherwise stall the apply goroutine every block) with
	// the CPU-bound apply. Bounded ring → a few MB, no RSS growth. With
	// --prefetch=0 the reader still runs but hands over one block at a time
	// (ring of 1 behaves like the old serial loop, minus the overlap).
	type fetchedBlock struct {
		n   uint32
		blk []byte // decompressed block (owned by this ring slot)
		raw []byte // compressed-bytes scratch (owned by this ring slot)
		err error
	}
	depth := *prefetch
	if depth < 1 {
		depth = 1
	}
	freeCh := make(chan fetchedBlock, depth)
	readyCh := make(chan fetchedBlock, depth)
	for i := 0; i < depth; i++ {
		freeCh <- fetchedBlock{}
	}
	fetchDone := make(chan struct{})
	defer close(fetchDone) // unblock the reader if apply bails out early
	go func() {
		defer close(readyCh)
		for n := uint32(*startBlock); n <= end; n++ {
			var slot fetchedBlock
			select {
			case slot = <-freeCh:
			case <-fetchDone:
				return
			}
			blk, newRaw, err := bl.BlockReuse(n, slot.raw, slot.blk)
			slot.raw = newRaw
			if blk != nil {
				// blk aliases slot.blk or is a fresh (bigger) alloc; either way
				// it becomes this slot's decompress buffer from now on.
				slot.blk = blk
			}
			select {
			case readyCh <- fetchedBlock{n: n, blk: blk, raw: slot.raw, err: err}:
			case <-fetchDone:
				return
			}
		}
	}()
	for fb := range readyCh {
		n := fb.n
		raw := fb.blk
		err := fb.err
		recycle := fetchedBlock{blk: fb.blk, raw: fb.raw}
		if err != nil {
			freeCh <- recycle
			fmt.Fprintf(os.Stderr, "block %d read: %v\n", n, err)
			errCount++
			if errCount >= uint64(*maxErrors) {
				return 1
			}
			continue
		}
		if _, err := cdb.ApplyBlock(raw, n, opts, &stats); err != nil {
			freeCh <- recycle
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
			// Keep the by-root cold gate correct across the HF17 boundary
			// (monotonic; cheap). Post-HF17 skips the dead by-root cold writes.
			commentStoreForEvict.SetColdByRootEnabled(!hardfork.SetForBlock(n).Has(hardfork.HF17))
			commentStoreForEvict.EvictFrozen(voteStoreForEvict, *evictBudget)
		}
		if *watchAccount != "" {
			cur := snapshotAccount(*watchAccount)
			if cur != prevSnap {
				fmt.Printf("  WATCH %s @block %d: bal %d→%d hbd %d→%d vests %d→%d savings %d→%d savings_hbd %d→%d rwd_vests %d→%d rwd_hive %d→%d rwd_hbd %d→%d\n",
					*watchAccount, n,
					prevSnap.balance, cur.balance,
					prevSnap.hbd, cur.hbd,
					prevSnap.vesting, cur.vesting,
					prevSnap.savings, cur.savings,
					prevSnap.savingsHBD, cur.savingsHBD,
					prevSnap.rwdVests, cur.rwdVests,
					prevSnap.rwdHive, cur.rwdHive,
					prevSnap.rwdHBD, cur.rwdHBD)
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
		headBlock = n
		// Cold-tier compaction: reclaim dead space (superseded records from
		// re-Put/promote) on long persist-backed runs. At a between-blocks safe
		// point (no held cold value slices) the remap is safe.
		if compactCold != nil && *coldCompactRatio > 0 && uint64(n)%uint64(*coldCompactEvery) == 0 {
			if dr := compactCold.DeadRatio(); dr > *coldCompactRatio {
				fmt.Printf("replay: cold dead-ratio %.2f > %.2f at block %d — compacting...\n", dr, *coldCompactRatio, n)
				cstart := time.Now()
				if err := compactCold.Compact(); err != nil {
					fmt.Fprintf(os.Stderr, "replay: cold compact: %v\n", err)
				} else {
					fmt.Printf("replay: cold compacted in %.1fs (dead-ratio now %.2f)\n", time.Since(cstart).Seconds(), compactCold.DeadRatio())
				}
			}
		}
		// The block's bytes are dead from here (state never retains aliases
		// past the apply+evict of its own block — same lifetime the old serial
		// loop gave the scratch buffers). Hand the slot back to the reader.
		freeCh <- recycle
		// Graceful stop requested by SIGTERM/SIGINT: the current block is fully
		// applied (headBlock == n), so break to the clean-shutdown path.
		if stopRequested.Load() {
			fmt.Printf("replay: clean stop after block %d\n", n)
			break
		}
	}

	// Zero-snapshot node: persist the full non-cold state into the persiststore
	// container (+ flush the durable cold tier). A restart with the same
	// --persist-state-dir + --cold-dir resumes at head — no snapshot file.
	if *persistStateDir != "" && headBlock >= uint32(*startBlock) {
		if compactCold != nil {
			if err := compactCold.Flush(); err != nil {
				fmt.Fprintf(os.Stderr, "replay: cold flush: %v\n", err)
			}
		}
		psStart := time.Now()
		if err := snapshot.SavePersist(stateDB, *persistStateDir, headBlock); err != nil {
			fmt.Fprintf(os.Stderr, "replay: save persist-state: %v\n", err)
		} else {
			fmt.Printf("replay: saved persist-state %s at block %d in %.1fs (cold flushed) — restart with --persist-state-dir=%s --cold-dir=%s resumes here\n",
				*persistStateDir, headBlock, time.Since(psStart).Seconds(), *persistStateDir, *coldDir)
		}
	}

	// Clean-shutdown head snapshot (on completion OR SIGTERM). With a durable
	// cold tier this is a fast resident-only dump; combined with flushing the
	// cold tier, a restart (--load-snapshot=<this> + same --cold-dir) resumes at
	// headBlock with ZERO replay tail.
	if *shutdownSnapshotDir != "" && headBlock >= uint32(*startBlock) {
		if compactCold != nil {
			if err := compactCold.Flush(); err != nil {
				fmt.Fprintf(os.Stderr, "replay: cold flush: %v\n", err)
			}
		}
		snapStart := time.Now()
		path, err := snapshot.Save(stateDB, *shutdownSnapshotDir, headBlock, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "replay: shutdown snapshot: %v\n", err)
		} else {
			fmt.Printf("replay: shutdown snapshot %s at block %d in %.1fs (cold flushed) — restart with --load-snapshot=%s --cold-dir=%s resumes here\n",
				path, headBlock, time.Since(snapStart).Seconds(), path, *coldDir)
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
	if *parallelApply > 0 {
		pa, pr := chain.ParallelStats()
		fmt.Printf("parallel:       %d txs applied speculatively, %d re-executed serially\n", pa, pr)
	}
	fmt.Printf("block errors:   %d\n", errCount)
	// hived's RC code warns (NOTIFYALERT) whenever an account's stored
	// last_max_rc disagrees with its computed maximum, and HF29's
	// RC_IS_CONSENSUS turns that warning into a throw. gopherd repairs the
	// value silently, so this count is the only way to know whether the
	// assertion would be safe to enable. A clean replay must report 0.
	if n := objects.RCStaleMaxCount(); n > 0 {
		fmt.Printf("rc stale max:   %d  (HF29 RC_IS_CONSENSUS would REJECT these blocks)\n", n)
		// The count says how often; only the call sites say whether it is an
		// ordering artifact (a site that mutates vesting then regenerates,
		// where hived would have regenerated first) or a real bookkeeping
		// hole (a read-only site finding the cache stale). Run with
		// GOPHERD_RC_ATTRIBUTION=1 to get this.
		if t := objects.FormatRCStaleCallSites(12); t != "" {
			fmt.Print(t)
		} else {
			fmt.Println("                (set GOPHERD_RC_ATTRIBUTION=1 to attribute these by call site)")
		}
	}
	// hived's OTHER HF29 assertion, counted separately: a post-change refresh
	// that arrived without a regeneration earlier in the block.
	if n := objects.RCNotRegeneratedCount(); n > 0 {
		fmt.Printf("rc not-regen:   %d  (hived's \"not regenerated prior to VEST change\")\n", n)
		if t := objects.FormatRCNotRegenCallSites(12); t != "" {
			fmt.Print(t)
		}
	}
	// hived caps each governance sweep at HIVE_GLOBAL_REMOVE_THRESHOLD
	// proposal-vote removals and defers the rest to a later block; gopherd
	// clears every due account at once. over>0 means the two genuinely disagree
	// about which block an account's votes were cleared in.
	if over, maxPer := chain.GovernanceRemovalStats(); over > 0 {
		fmt.Printf("govsweep budget: %d sweeps over the %d-object limit (max %d in one sweep) — gopherd and hived DIVERGE here\n",
			over, network.GlobalRemoveThreshold, maxPer)
	} else if maxPer > 0 {
		fmt.Printf("govsweep budget: OK (max %d proposal votes in one sweep, limit %d)\n",
			maxPer, network.GlobalRemoveThreshold)
	}
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
