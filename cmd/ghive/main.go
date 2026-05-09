package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

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
)

func main() {
	var (
		showVersion bool
		showNetwork bool
		configPath  string
		dataDir     string
		replay      bool
	)
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.BoolVar(&showNetwork, "network", false, "print compiled-in network constants and exit")
	flag.StringVar(&configPath, "config", "", "path to config.ini")
	flag.StringVar(&dataDir, "data-dir", "", "data directory (overrides config.ini)")
	flag.BoolVar(&replay, "replay-blockchain", false, "replay the block_log into a fresh state dir before starting")
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

	stateDB, err := state.Open(cfg.DataDir, 256<<20)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ghive: open state: %v\n", err)
		os.Exit(1)
	}
	// Coalesce per-block backend writes into batches of N. Crash recovery
	// re-replays from the block_log writer's head so durability is
	// preserved across kill -9. Default 100 picked to keep per-block read
	// cost low while still amortizing fsync over a meaningful window.
	stateDB.SetBatchSize(100)
	defer stateDB.Close()

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

	cdb := chain.New(stateDB)
	// Enable in-memory recent-block cache so the p2p sync layer can
	// re-serve fetch_items requests without going to disk.
	cdb.EnableRecentBlockCache(chain.RecentBlockCacheSize)

	// Crash recovery: a hard kill mid-batch can leave the persisted
	// state up to BatchSize-1 blocks behind the local block_log (which
	// fsyncs on every Append). Re-replay any gap before we accept peer
	// or witness traffic.
	blockLogPath := filepath.Join(cfg.DataDir, "blockchain", "block_log")
	if err := recoverFromBlockLog(cdb, blockLogPath); err != nil {
		fmt.Fprintf(os.Stderr, "ghive: crash recovery: %v\n", err)
		os.Exit(1)
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
		syncDel *p2psync.Delegate
	)
	if cfg.P2PEndpoint != "" || len(cfg.P2PSeedNodes) > 0 {
		// Build the Node first (gossip + sync both need a Node handle to
		// broadcast on); wire delegates after construction via
		// SetDelegate so we can layer them in a MultiDelegate.
		p2p = p2pnode.New(p2pnode.Config{
			Endpoint:   cfg.P2PEndpoint,
			SeedNodes:  cfg.P2PSeedNodes,
			UserAgent:  "ghive/0.1.0",
			PeerDBPath: filepath.Join(cfg.DataDir, peerdb.Filename),
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

	// Wait for interrupt.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	fmt.Println("ghive: shutting down...")
	// Flush the commit batch so the on-disk state catches up to the
	// in-memory head before Close. Without this a graceful shutdown
	// loses up to BatchSize-1 blocks of state — the chain re-derives
	// them from the block_log on next start, but we'd rather not pay
	// that cost when we have a clean exit window.
	if err := cdb.FlushBatch(true); err != nil {
		fmt.Fprintf(os.Stderr, "ghive: flush batch: %v\n", err)
	}
}

// recoverFromBlockLog replays any blocks present in the local block_log
// but missing from the chain.Database state. With Database.BatchSize>1
// a hard kill can leave the persisted backend behind the durably-written
// block_log; this pass closes the gap before normal startup proceeds.
//
// No-op when the block_log is missing (fresh data dir) or the state
// head already matches.
func recoverFromBlockLog(cdb *chain.Database, blockLogPath string) error {
	if _, err := os.Stat(blockLogPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	stateHead, err := cdb.HeadBlock()
	if err != nil {
		return fmt.Errorf("read state head: %w", err)
	}
	r, err := blocklog.Open(blockLogPath)
	if err != nil {
		return fmt.Errorf("open block_log: %w", err)
	}
	defer r.Close()
	logHead := r.Head()
	if logHead == 0 || logHead <= stateHead {
		return nil
	}
	fmt.Printf("ghive: crash recovery: state head=%d, block_log head=%d; replaying %d blocks\n",
		stateHead, logHead, logHead-stateHead)
	opts := chain.ApplyOptions{
		Sync:                      false,
		SkipBlockSignature:        true,
		SkipTransactionSignatures: true,
	}
	for n := stateHead + 1; n <= logHead; n++ {
		raw, err := r.Block(n)
		if err != nil {
			return fmt.Errorf("read block %d: %w", n, err)
		}
		if _, err := cdb.ApplyBlock(raw, n, opts, nil); err != nil {
			return fmt.Errorf("apply block %d: %w", n, err)
		}
	}
	if err := cdb.FlushBatch(true); err != nil {
		return fmt.Errorf("flush after recovery: %w", err)
	}
	fmt.Printf("ghive: crash recovery: state caught up to block %d\n", logHead)
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
