package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

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
	)
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.BoolVar(&showNetwork, "network", false, "print compiled-in network constants and exit")
	flag.StringVar(&configPath, "config", "", "path to config.ini")
	flag.StringVar(&dataDir, "data-dir", "", "data directory (overrides config.ini)")
	flag.BoolVar(&replay, "replay-blockchain", false, "replay the block_log into a fresh state dir before starting")
	flag.IntVar(&cacheMB, "cache-mb", 1024, "pebble block-cache size in MB (default 1024; was 256 hard-coded)")
	flag.StringVar(&loadSnapshot, "load-snapshot", "", "load a state snapshot (single-file or directory) on startup before genesis/sync")
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
		syncDel *p2psync.Delegate
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
		// Pipe local block-apply events into the sync layer so it wakes
		// its INV broadcaster the moment a witness produces a block —
		// otherwise the new block waits up to one INVInterval before
		// peers learn of it.
		cdb.SetOnBlockApplied(func(_ uint32, _ block.ID) {
			syncDel.NotifyHeadAdvanced()
		})
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
