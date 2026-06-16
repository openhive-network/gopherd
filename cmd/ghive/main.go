package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"syscall"
	"time"

	"hived-go/internal/blocklog"
	"hived-go/internal/chain"
	"hived-go/internal/config"
	"hived-go/internal/crypto"
	"hived-go/internal/p2p/gossip"
	p2pnode "hived-go/internal/p2p/node"
	"hived-go/internal/p2p/peerdb"
	p2psync "hived-go/internal/p2p/sync"
	"hived-go/internal/plugins/witness"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/network"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
	"hived-go/internal/state/snapshot"
)

func main() {
	var (
		showVersion  bool
		showNetwork  bool
		configPath   string
		dataDir      string
		replay       bool
		cacheMB      int
		loadSnapshot string
		residentCap  int
		evictBudget  int
		compressLog  bool
	)
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.BoolVar(&showNetwork, "network", false, "print compiled-in network constants and exit")
	flag.StringVar(&configPath, "config", "", "path to config.ini")
	flag.StringVar(&dataDir, "data-dir", "", "data directory (overrides config.ini)")
	flag.BoolVar(&replay, "replay-blockchain", false, "replay the block_log into a fresh state dir before starting")
	flag.IntVar(&cacheMB, "cache-mb", 1024, "pebble block-cache size in MB (default 1024; was 256 hard-coded)")
	flag.StringVar(&loadSnapshot, "load-snapshot", "", "load a state snapshot (single-file or directory) on startup before genesis/sync")
	flag.IntVar(&residentCap, "comment-resident-cap", 2_000_000, "cap RAM-resident comments; frozen comments + their votes spill to an on-disk cold tier (the Go equivalent of hived's mmap'd chainbase paging). 0 disables (unbounded RAM). Pair with GOMEMLIMIT to also cap RSS — at 13M blocks this took RSS ~9.6G→4.1G.")
	flag.IntVar(&evictBudget, "evict-budget", 20000, "max comments evicted to the cold tier per block when over the resident cap")
	flag.BoolVar(&compressLog, "compress-block-log", false, "zstd-compress the local block_log (matches hived's on-disk format, ~3-4x smaller). The whole log must be one format; switching requires a fresh log.")
	flag.Parse()

	if showVersion {
		fmt.Println("ghive 0.1.0 (P0 → P1 in progress)")
		return
	}
	if showNetwork {
		fmt.Printf("network=%s address-prefix=%s chain-id=%s\n",
			network.Name, network.AddressPrefix, network.ChainID.Hex())
		return
	}

	var cfg *config.Config
	if configPath != "" {
		c, err := config.Load(configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ghive: config: %v\n", err)
			os.Exit(1)
		}
		// Refuse to launch on any plugin gopherd doesn't implement —
		// silently ignoring a plugin hived would run risks divergence.
		// The config FORMAT stays interchangeable with hived (unknown
		// keys are tolerated); only unsupported `plugin =` entries are
		// fatal.
		if err := c.ValidatePlugins(); err != nil {
			fmt.Fprintf(os.Stderr, "ghive: %v\n", err)
			os.Exit(1)
		}
		cfg = c
	} else {
		cfg = &config.Config{}
	}
	if dataDir != "" {
		cfg.DataDir = dataDir
	}
	if cfg.DataDir == "" {
		fmt.Fprintln(os.Stderr, "ghive: --data-dir or 'data-dir = ...' in config.ini is required")
		os.Exit(2)
	}

	// Heap-profile-on-SIGUSR2 (registered EARLY so it works during replay
	// too): writes a runtime/pprof heap profile to $GHIVE_HEAP_PROFILE
	// (default /tmp/ghive-heap.pprof). Inspect with
	// `go tool pprof -inuse_space -top <file>`.
	heapCh := make(chan os.Signal, 1)
	signal.Notify(heapCh, syscall.SIGUSR2)
	go func() {
		for range heapCh {
			path := os.Getenv("GHIVE_HEAP_PROFILE")
			if path == "" {
				path = "/tmp/ghive-heap.pprof"
			}
			f, err := os.Create(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ghive: SIGUSR2: create heap profile: %v\n", err)
				continue
			}
			runtime.GC()
			if err := pprof.WriteHeapProfile(f); err != nil {
				fmt.Fprintf(os.Stderr, "ghive: SIGUSR2: write heap profile: %v\n", err)
			} else {
				fmt.Printf("ghive: heap profile written to %s\n", path)
			}
			_ = f.Close()
		}
	}()

	// --replay-blockchain rebuilds state deterministically from the local
	// block_log. The typed stores (Account/Witness/Comment/...) are
	// in-memory only, so a fresh replay must start from a clean pebble dir;
	// wipe the K/V files (preserving blockchain/ + peerdb) before reopening.
	// Mirrors hived, which rebuilds its state index from the block_log.
	if replay {
		if err := wipePebbleFiles(cfg.DataDir); err != nil {
			fmt.Fprintf(os.Stderr, "ghive: wipe state for replay: %v\n", err)
			os.Exit(1)
		}
	}

	cacheBytes := int64(cacheMB) << 20
	if cacheBytes <= 0 {
		cacheBytes = 1024 << 20
	}
	stateDB, err := state.Open(cfg.DataDir, cacheBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ghive: open state: %v\n", err)
		os.Exit(1)
	}
	defer stateDB.Close()

	// Pre-register every typed subsystem on the freshly-opened Database
	// so a snapshot load finds them by key. Mirrors the boot section in
	// ghive-tools/replay.go.
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

	// Snapshot load happens BEFORE genesis. If the snapshot covers a
	// post-genesis state, we skip genesis.Apply (which would clobber
	// initminer's mana, DGPO, etc.).
	if loadSnapshot != "" {
		st, err := os.Stat(loadSnapshot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ghive: stat snapshot: %v\n", err)
			os.Exit(1)
		}
		if st.IsDir() {
			man, err := snapshot.Load(stateDB, loadSnapshot)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ghive: load snapshot dir: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("ghive: loaded snapshot dir %s at block %d\n", loadSnapshot, man.BlockNum)
		} else {
			h, err := stateDB.LoadSnapshot(loadSnapshot)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ghive: load snapshot: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("ghive: loaded snapshot %s at block %d\n", loadSnapshot, h)
		}
	} else {
		// Bootstrap: if the state dir is empty, apply genesis. Selects
		// mainnet vs testnet variant via config-ini `genesis-type`.
		switch cfg.GenesisType {
		case "testnet":
			if err := genesis.ApplyTestnet(stateDB, 21); err != nil {
				_ = err
			}
			fmt.Println("ghive: testnet genesis applied (21 init witnesses + 21 operator accounts)")
		default:
			if err := genesis.Apply(stateDB); err != nil {
				// genesis.Apply tolerates an already-bootstrapped dir
				// (writes are idempotent).
				_ = err
			}
		}
	}

	cdb := chain.New(stateDB)
	// Enable in-memory recent-block cache so the p2p sync layer can
	// re-serve fetch_items requests without going to disk.
	cdb.EnableRecentBlockCache(chain.RecentBlockCacheSize)

	// Cold tier: spill frozen comments + their votes to an on-disk scratch
	// store so the RAM working set stays bounded. This is the Go port's
	// equivalent of hived's chainbase, whose state lives in a memory-mapped
	// file the OS pages in/out — without it the in-heap typed stores grow
	// without bound (tens of millions of comments+votes by mid-chain). The
	// cold dir lives under the data-dir, so --replay-blockchain wipes and
	// rebuilds it along with the rest of derived state.
	var (
		commentStore *objects.CommentStore
		voteStore    *objects.CommentVoteStore
	)
	if residentCap > 0 {
		coldDir := filepath.Join(cfg.DataDir, "comment_cold")
		cold, cerr := objects.OpenColdKV(coldDir)
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "ghive: open cold tier: %v\n", cerr)
			os.Exit(1)
		}
		defer cold.Close()
		boot := state.NewSession(stateDB)
		commentStore = objects.EnsureCommentStore(boot)
		voteStore = objects.EnsureCommentVoteStore(boot)
		commentStore.SetColdTier(cold)
		commentStore.SetResidentCap(residentCap)
		voteStore.SetColdTier(cold)
		fmt.Printf("ghive: cold tier ON — resident cap %d comments, evict budget %d/block, dir %s\n",
			residentCap, evictBudget, cold.Dir())
	} else {
		fmt.Println("ghive: cold tier OFF (--comment-resident-cap=0) — RAM-resident state is unbounded")
	}

	// Local block_log writer. Blocks applied from p2p (and locally
	// produced blocks) are appended here so a later --replay-blockchain can
	// rebuild state from disk — the same on-disk role as hived's block_log.
	// The append is guarded to the strictly-next block so the replay below
	// (which reads FROM this log) never double-writes.
	blockchainDir := filepath.Join(cfg.DataDir, "blockchain")
	if err := os.MkdirAll(blockchainDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "ghive: mkdir blockchain: %v\n", err)
		os.Exit(1)
	}
	blockLogPath := filepath.Join(blockchainDir, "block_log")
	writer, err := blocklog.OpenForWrite(blockLogPath, compressLog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ghive: open block_log writer: %v\n", err)
		os.Exit(1)
	}
	defer writer.Close()

	// syncDel is wired below once p2p starts; this apply hook references it
	// (nil during the replay phase, which is fine). The hook (1) persists
	// each freshly-applied block to the local block_log and (2) wakes the
	// sync INV broadcaster so a locally-produced block propagates promptly.
	var syncDel *p2psync.Delegate
	cdb.SetOnBlockApplied(func(blockNum uint32, _ block.ID) {
		// Persist to the local block_log. Guard to the strictly-next block
		// so replayed/duplicate applies (at or below the writer head) don't
		// double-append; only genuinely new blocks extend the log.
		if blockNum == writer.HeadBlock()+1 {
			if raw := cdb.RecentBlockBytes(blockNum); raw != nil {
				if err := writer.Append(raw); err != nil {
					fmt.Fprintf(os.Stderr, "ghive: block_log append %d: %v\n", blockNum, err)
				}
			}
		}
		if syncDel != nil {
			syncDel.NotifyHeadAdvanced()
		}
		// Post-commit safe point (still under the apply lock): spill frozen
		// comments + votes to the cold tier when over the resident cap.
		// No-op when the cold tier is off or we're under the cap.
		if commentStore != nil {
			commentStore.EvictFrozen(voteStore, evictBudget)
		}
	})

	// --replay-blockchain: rebuild state from the local block_log before
	// going live. Afterwards the p2p layer picks up from the replayed head
	// and syncs the remainder from peers (replay → p2p handoff, like hived).
	if replay {
		if err := replayBlockLog(cdb, blockLogPath); err != nil {
			fmt.Fprintf(os.Stderr, "ghive: replay: %v\n", err)
			os.Exit(1)
		}
	}

	// Load witness producers from config. Each (witness, private-key)
	// pair becomes a Producer entry. Without producers the daemon is
	// observer-only.
	var producers []witness.Producer
	for i := 0; i < len(cfg.Witnesses) && i < len(cfg.PrivateKeys); i++ {
		priv, err := parsePrivateKey(cfg.PrivateKeys[i])
		if err != nil {
			fmt.Fprintf(os.Stderr, "ghive: witness %q: %v\n", cfg.Witnesses[i], err)
			os.Exit(1)
		}
		pub := priv.PublicKey()
		producers = append(producers, witness.Producer{
			Account:     cfg.Witnesses[i],
			PrivateKey:  priv,
			PublicKey33: pub,
		})
	}
	wp := witness.New(cdb, stateDB, producers)
	// wp.Start is deferred until after the gossip+p2p wiring below so
	// the broadcaster handle is in place before the production loop
	// possibly fires its first APPLIED.

	// Sanity-check producer pubkeys against the witness_object on chain
	// so misconfiguration is caught at startup rather than every block.
	if len(producers) > 0 {
		sess := state.NewSession(stateDB)
		for _, p := range producers {
			w, err := objects.GetWitness(sess, auth.AccountName(p.Account))
			if err != nil {
				fmt.Fprintf(os.Stderr, "ghive: warning: witness %q has no on-chain witness object yet (%v); produced blocks will be rejected\n", p.Account, err)
				continue
			}
			if w.BlockSigningKey != p.PublicKey33 {
				fmt.Fprintf(os.Stderr, "ghive: warning: configured key for %q (%s) does not match on-chain BlockSigningKey (%s); produced blocks will be rejected\n",
					p.Account, p.PublicKey33.EncodeBase58(), w.BlockSigningKey.EncodeBase58())
			}
		}
	}

	if len(producers) > 0 {
		fmt.Printf("ghive: starting with %d witness producer(s)\n", len(producers))
	} else {
		fmt.Println("ghive: starting in observer mode (no witnesses configured)")
	}

	// P2P + sync + gossip. If no endpoint AND no seeds, skip entirely.
	// Otherwise stack both delegates onto a MultiDelegate: sync handles
	// catch-up + INV exchange; gossip handles direct block/tx broadcast
	// and the local-production fan-out.
	var (
		p2p     *p2pnode.Node
		gossipS *gossip.Service
	)
	if cfg.P2PEndpoint != "" || len(cfg.P2PSeedNodes) > 0 {
		// Build the Node first (gossip + sync both need a Node handle to
		// broadcast on); wire delegates after construction via
		// SetDelegate so we can layer them in a MultiDelegate.
		p2p = p2pnode.New(p2pnode.Config{
			Endpoint:           cfg.P2PEndpoint,
			SeedNodes:          cfg.P2PSeedNodes,
			UserAgent:          "ghive/0.1.0",
			PeerDBPath:         filepath.Join(cfg.DataDir, peerdb.Filename),
			MaxOutboundPeers:   cfg.P2PMaxOutboundPeers,
			MaxConcurrentDials: cfg.P2PMaxConcurrentDials,
		}, cdb, nil)
		gossipS = gossip.New(gossip.Config{
			Chain:   cdb,
			Node:    p2p,
			Mempool: wp,
		})
		// sync owns the SINGLE apply goroutine for chain.Database (see
		// internal/p2p/sync/loops.go applyLoop). Gossip's own apply
		// loop used to share the chain.Database session and racing it
		// against sync's loop corrupted state.Session's maps. The fix
		// is to delete gossip's apply path and have sync invoke
		// gossip.BroadcastAppliedBlock from inside its applyLoop —
		// re-broadcast then runs on the same goroutine that committed
		// the block.
		syncDel = p2psync.New(p2psync.Config{
			OnBlockApplied: gossipS.BroadcastAppliedBlock,
		}, cdb)
		// The block-apply hook installed above (block_log append + INV
		// wake) now sees this non-nil syncDel and wakes its INV broadcaster
		// the moment a witness produces a block, so the new block doesn't
		// wait up to one INVInterval before peers learn of it.
		p2p.SetDelegate(p2pnode.NewMultiDelegate(syncDel, gossipS))
		if err := p2p.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "ghive: p2p start: %v\n", err)
			os.Exit(1)
		}
		syncDel.Start()
		defer syncDel.Stop()
		defer p2p.Stop()
		// Wire the witness loop to push freshly-produced blocks at the
		// gossip layer.
		wp.Broadcaster = gossipS
	}

	// Now that the broadcaster is in place, start the production loop.
	wp.Start()
	defer wp.Stop()

	// Periodic head-block logger so an outside watcher can see progress
	// during a long p2p sync. Logs once a second; cheap (1 DGPO read).
	stopHB := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var last uint32
		for {
			select {
			case <-stopHB:
				return
			case <-t.C:
				if h, err := cdb.HeadBlock(); err == nil && h != last {
					fmt.Printf("ghive: head=%d\n", h)
					last = h
				}
			}
		}
	}()

	// Snapshot-on-SIGUSR1: writes a single-file snapshot to
	// ${GHIVE_SNAPSHOT_DIR}/p2p-block-<head>.snap when the signal is
	// received. Allows an outside loop to periodically checkpoint the
	// state without restarting the daemon. The Pebble layer is safe for
	// concurrent reads during apply, so the snapshot just races at the
	// row level — any per-block partial-write window is small relative
	// to the time to scan all rows.
	usrCh := make(chan os.Signal, 1)
	signal.Notify(usrCh, syscall.SIGUSR1)
	go func() {
		for range usrCh {
			snapDir := os.Getenv("GHIVE_SNAPSHOT_DIR")
			if snapDir == "" {
				fmt.Fprintln(os.Stderr, "ghive: SIGUSR1: GHIVE_SNAPSHOT_DIR not set; skipping")
				continue
			}
			// Serialize the snapshot against the apply goroutine. The
			// typed stores (DGPOStore, AccountStore, etc.) use Go maps
			// internally — concurrent read from snapshot.Save while
			// ApplyBlock writes them crashes the runtime with
			// "concurrent map read and map write". WithApplyLock holds
			// the apply write lock for the duration; apply pauses
			// briefly while the snapshot streams.
			var (
				path  string
				saveE error
				h     uint32
			)
			cdb.WithApplyLock(func() {
				var err error
				h, err = cdb.HeadBlock()
				if err != nil {
					saveE = fmt.Errorf("read head: %w", err)
					return
				}
				headID := ""
				if id, err := cdb.HeadBlockID(); err == nil {
					headID = fmt.Sprintf("%x", id[:])
				}
				path, saveE = snapshot.Save(stateDB, snapDir, h, headID)
			})
			if saveE != nil {
				fmt.Fprintf(os.Stderr, "ghive: SIGUSR1: save snapshot: %v\n", saveE)
				continue
			}
			fmt.Printf("ghive: snapshot saved %s (head=%d)\n", path, h)
		}
	}()

	// Wait for interrupt.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	close(stopHB)
	fmt.Println("ghive: shutting down...")
}

// replayBlockLog applies every block in the local block_log from the
// current state head forward, rebuilding the in-memory typed stores and
// pebble K/V. Uses batched commits + skipped signatures (the canonical
// local bytes already passed validation when first written) for speed,
// then restores per-block durable commit for live operation. After it
// returns, the p2p layer catches up from the replayed head to the live
// tip — the replay → p2p handoff.
func replayBlockLog(cdb *chain.Database, path string) error {
	r, err := blocklog.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("ghive: replay: no local block_log; nothing to replay")
			return nil
		}
		return fmt.Errorf("open block_log: %w", err)
	}
	defer r.Close()
	target := r.Head()
	head, _ := cdb.HeadBlock()
	if target <= head {
		fmt.Printf("ghive: replay: nothing to do (block_log head %d <= state head %d)\n", target, head)
		return nil
	}
	cdb.SetCommitBatch(1000, true)
	fmt.Printf("ghive: replay: applying blocks %d..%d from %s\n", head+1, target, path)
	start := time.Now()
	for n := head + 1; n <= target; n++ {
		raw, err := r.Block(n)
		if err != nil {
			return fmt.Errorf("read block %d: %w", n, err)
		}
		if _, err := cdb.ApplyBlock(raw, n, chain.ApplyOptions{
			SkipBlockSignature:        true,
			SkipTransactionSignatures: true,
			Sync:                      false,
		}, nil); err != nil {
			return fmt.Errorf("apply block %d: %w", n, err)
		}
		if n%100000 == 0 {
			el := time.Since(start).Seconds()
			fmt.Printf("ghive: replay: head=%d (%.0f bps)\n", n, float64(n-head)/el)
		}
	}
	if err := cdb.FlushBatch(true); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	// Restore per-block durable commit for live operation.
	cdb.SetCommitBatch(1, true)
	el := time.Since(start).Seconds()
	fmt.Printf("ghive: replay: done, head=%d in %.1fs (%.0f bps)\n", target, el, float64(target-head)/el)
	return nil
}

// wipePebbleFiles removes pebble's state files from dir while preserving
// the blockchain/ subtree (block_log + index) and the peerdb. Used before
// a --replay-blockchain rebuild: the typed stores are in-memory only, so a
// deterministic replay must start from clean K/V. Mirrors the same helper
// in cmd/ghive-sync.
func wipePebbleFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		switch e.Name() {
		case "blockchain", "peerdb":
			continue // preserve
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// parsePrivateKey decodes a config.ini `private-key` string in WIF format
// (base58check, 0x80-prefix, 32-byte secret) into a *crypto.PrivateKey.
// Mirrors hived's `fc::ecc::private_key::wif_to_key`.
func parsePrivateKey(wif string) (*crypto.PrivateKey, error) {
	if wif == "" {
		return nil, fmt.Errorf("config: empty private-key")
	}
	priv, err := crypto.DecodeWIF(wif)
	if err != nil {
		return nil, fmt.Errorf("config: parse WIF private-key: %w", err)
	}
	return priv, nil
}
