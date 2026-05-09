// Command ghive-sync is the live-mainnet sync skeleton.
//
// It dials hived peers, downloads blocks via the P2P+sync code, validates
// each block (signature + merkle + every op evaluator), and appends the raw
// block bytes to a local block_log that should be byte-identical to what
// hived would produce.
//
// What's wired today:
//
//   - State dir bootstrap (mainnet genesis if empty).
//   - Block_log writer at <data-dir>/blockchain/block_log (uncompressed,
//     monolithic — see internal/blocklog/writer.go for the limitations).
//   - P2P node + mainnet seed dial.
//   - Self-test path that re-applies blocks from an existing block_log
//     (--source-block-log) and round-trips them via the writer; this
//     exercises the full validate→append loop without needing real peers.
//   - --target-block / --verify-against / --max-blocks bounds for testing.
//
// What's NOT yet wired (waits on agent-sync):
//
//   - The sync delegate hook that the P2P node calls per received block.
//     Once agent-sync exposes an OnBlockReceived(*Peer, rawBlock []byte) hook
//     (see TODO in run()), the only change here is to plumb our writeBlock
//     callback into the delegate.
//
// Compile: `go build ./cmd/ghive-sync`.
// Quick self-test: `go test ./cmd/ghive-sync/`.
// Live run (once sync agent lands): `ghive-sync --data-dir /tmp/sync-state`.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"sync/atomic"
	"syscall"
	"time"

	"hived-go/internal/blocklog"
	"hived-go/internal/chain"
	p2pnode "hived-go/internal/p2p/node"
	p2psync "hived-go/internal/p2p/sync"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/network"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
)

func main() {
	cfg := parseFlags()

	if cfg.showSeeds {
		for _, s := range network.MainnetSeeds {
			fmt.Println(s)
		}
		return
	}

	if cfg.dataDir == "" {
		fmt.Fprintln(os.Stderr, "ghive-sync: --data-dir is required")
		os.Exit(2)
	}

	app, err := newApp(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ghive-sync: %v\n", err)
		os.Exit(1)
	}
	defer app.close()

	if cfg.sourceBlockLog != "" {
		// Self-replay path. No peers, no sync — just stream blocks from
		// the source block_log into the validator + writer. This is what
		// the unit test in main_test.go exercises end-to-end.
		if err := app.replayFromSource(cfg.sourceBlockLog, cfg.maxBlocks); err != nil {
			fmt.Fprintf(os.Stderr, "ghive-sync: replay: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := app.run(); err != nil {
		fmt.Fprintf(os.Stderr, "ghive-sync: %v\n", err)
		os.Exit(1)
	}
}

type flags struct {
	dataDir          string
	targetBlock      uint32
	verifyAgainst    string
	sourceBlockLog   string
	maxBlocks        uint32
	showSeeds        bool
	listenEndpoint   string
	extraSeeds       string
	disableMainSeeds bool
	// compressBlocks controls whether the on-disk block_log we write is
	// zstd-compressed (per-block, with the hived dictionary scheme) or
	// stored uncompressed. Off by default to keep existing tests +
	// downstream tooling that compares byte-identical against an
	// uncompressed reference unchanged.
	compressBlocks bool
	// backend selects the state backend. "pebble" (default, persistent)
	// or "mem" (in-memory btree — volatile, ~3-5x faster, suitable for
	// sync-verification runs that reset state on restart).
	backend string
	// cpuProfile, when non-empty, is the path to write a runtime/pprof
	// CPU profile to. The profile is started at app boot and stopped on
	// shutdown.
	cpuProfile string
	// noVerifyMerkle disables the per-block merkle-root recomputation
	// inside validateBlock. Saves ~5% CPU on the apply path; only safe
	// if you trust the canonical block_log reference (which already
	// validated the merkle root upstream).
	noVerifyMerkle bool
	// maxOutboundPeers caps active outbound peer count. 0 → use the
	// p2pnode default (32). Bigger value = more parallel block fetches
	// but more sockets + more INV traffic.
	maxOutboundPeers int
	// maxConcurrentDials caps in-flight dial attempts at any moment.
	// 0 → p2pnode default (24). Tied to maxOutboundPeers — needs to be
	// large enough to ramp the peer set up quickly once peerdb fills.
	maxConcurrentDials int
}

func parseFlags() flags {
	var f flags
	var target, max uint
	flag.StringVar(&f.dataDir, "data-dir", "", "data directory (state + blockchain/block_log live here)")
	flag.UintVar(&target, "target-block", 0, "stop sync after applying this block number (0 = unlimited)")
	flag.StringVar(&f.verifyAgainst, "verify-against", "", "path to a reference block_log; assert each applied block's bytes match")
	flag.StringVar(&f.sourceBlockLog, "source-block-log", "", "self-replay mode: read blocks from this block_log instead of P2P")
	flag.UintVar(&max, "max-blocks", 0, "in self-replay mode, stop after this many blocks (0 = the source's head)")
	flag.BoolVar(&f.showSeeds, "show-seeds", false, "print the compiled-in mainnet seed list and exit")
	flag.StringVar(&f.listenEndpoint, "p2p-endpoint", "", "local listen address (host:port); empty = outbound-only")
	flag.StringVar(&f.extraSeeds, "extra-seed", "", "additional seed (host:port); pass --extra-seed multiple times to add more")
	flag.BoolVar(&f.disableMainSeeds, "no-default-seeds", false, "skip the compiled-in mainnet seeds; only --extra-seed peers will be dialed")
	flag.BoolVar(&f.compressBlocks, "compress-blocks", false, "zstd-compress each block in the local block_log (matches hived's on-disk format byte-for-byte)")
	flag.StringVar(&f.backend, "backend", "", "state backend: pebble (persistent) or mem (volatile, ~3-5x faster); default = pebble unless --verify-against is set, in which case mem (since the run is throwaway anyway)")
	flag.StringVar(&f.cpuProfile, "cpuprofile", "", "write runtime/pprof CPU profile to this path")
	flag.BoolVar(&f.noVerifyMerkle, "no-verify-merkle", false, "skip the per-block merkle-root recomputation (faster; only safe with --verify-against)")
	flag.IntVar(&f.maxOutboundPeers, "max-outbound-peers", 0, "max active outbound P2P peers (0 = p2pnode default, currently 32)")
	flag.IntVar(&f.maxConcurrentDials, "max-concurrent-dials", 0, "max in-flight dial attempts (0 = p2pnode default, currently 24)")
	flag.Parse()
	f.targetBlock = uint32(target)
	f.maxBlocks = uint32(max)
	// Default the state backend based on whether the caller is doing a
	// verify-against run. Verify-against runs throw away state on
	// completion (the only output that matters is the byte-for-byte
	// block_log diff against the reference), so the persistent pebble
	// backend's per-block fsync is wasted; mem is ~3-5x faster.
	// Persistent runs (no --verify-against) keep pebble for crash safety.
	if f.backend == "" {
		if f.verifyAgainst != "" {
			f.backend = "mem"
		} else {
			f.backend = "pebble"
		}
	}
	return f
}

// app bundles the long-lived per-process resources.
type app struct {
	cfg    flags
	state  *state.Database
	chain  *chain.Database
	writer blocklog.Writer
	verify *blocklog.Reader // optional, for --verify-against
	p2p    *p2pnode.Node
	logger *appLogger

	// progress tracking — afterApply rate-limits its log line to one
	// emission per progressInterval (or per progressBlocks blocks).
	progressBlocks   uint32
	progressInterval time.Duration
	progressLastT    atomic.Int64
	progressLastN    atomic.Uint32
	progressStartT   time.Time
	progressStartN   atomic.Uint32

	// cpuProfileFile, when non-nil, is the open profile file; closed by
	// app.close() AFTER pprof.StopCPUProfile is called.
	cpuProfileFile *os.File

	// skipSigsForTest is set by the test harness to bypass block + tx
	// signature checks. The production live-sync path leaves it false:
	// hived peers serve real blocks, the witness signature has to verify,
	// otherwise we'd happily archive forged blocks. Tests against
	// historical mainnet bytes reuse the same applyBlock flow but flip
	// this since there's no signing-key infrastructure in unit tests.
	skipSigsForTest bool
}

func newApp(cfg flags) (*app, error) {
	var stateDB *state.Database
	switch cfg.backend {
	case "mem":
		stateDB = state.OpenMem()
	case "", "pebble":
		var err error
		stateDB, err = state.Open(cfg.dataDir, 256<<20)
		if err != nil {
			return nil, fmt.Errorf("open state %s: %w", cfg.dataDir, err)
		}
	default:
		return nil, fmt.Errorf("unknown --backend %q (want pebble or mem)", cfg.backend)
	}
	if err := genesis.Apply(stateDB); err != nil {
		// Apply tolerates an already-bootstrapped dir.
		_ = err
	}
	cdb := chain.New(stateDB)
	// Enable the recent-block cache so afterApply can pull each freshly-
	// applied block's raw bytes back out and stream them into the writer.
	// Without this, RecentBlockBytes returns nil and our block_log stays
	// empty even though every block validates.
	cdb.EnableRecentBlockCache(2048)

	blockchainDir := filepath.Join(cfg.dataDir, "blockchain")
	if err := os.MkdirAll(blockchainDir, 0o755); err != nil {
		stateDB.Close()
		return nil, fmt.Errorf("mkdir blockchain: %w", err)
	}
	blockLogPath := filepath.Join(blockchainDir, "block_log")
	w, err := blocklog.OpenForWrite(blockLogPath, cfg.compressBlocks)
	if err != nil {
		stateDB.Close()
		return nil, fmt.Errorf("open block_log writer: %w", err)
	}

	a := &app{
		cfg:              cfg,
		state:            stateDB,
		chain:            cdb,
		writer:           w,
		logger:           newAppLogger(os.Stderr),
		progressBlocks:   1000,
		progressInterval: time.Second,
		progressStartT:   time.Now(),
	}
	a.progressLastT.Store(a.progressStartT.UnixNano())

	if cfg.cpuProfile != "" {
		f, err := os.Create(cfg.cpuProfile)
		if err != nil {
			a.close()
			return nil, fmt.Errorf("create cpuprofile %s: %w", cfg.cpuProfile, err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			f.Close()
			a.close()
			return nil, fmt.Errorf("start cpuprofile: %w", err)
		}
		a.cpuProfileFile = f
		a.logger.printf("cpu profile -> %s", cfg.cpuProfile)
	}

	if cfg.verifyAgainst != "" {
		ref, err := blocklog.Open(cfg.verifyAgainst)
		if err != nil {
			a.close()
			return nil, fmt.Errorf("open --verify-against %s: %w", cfg.verifyAgainst, err)
		}
		a.verify = ref
	}
	return a, nil
}

func (a *app) close() {
	if a.p2p != nil {
		a.p2p.Stop()
		a.p2p = nil
	}
	if a.writer != nil {
		_ = a.writer.Close()
		a.writer = nil
	}
	if a.verify != nil {
		_ = a.verify.Close()
		a.verify = nil
	}
	if a.state != nil {
		_ = a.state.Close()
		a.state = nil
	}
	if a.cpuProfileFile != nil {
		pprof.StopCPUProfile()
		_ = a.cpuProfileFile.Close()
		a.cpuProfileFile = nil
	}
}

// run brings up the P2P node + sync delegate and waits for SIGINT/SIGTERM
// (or, when --target-block is set, until the chain head reaches it).
//
// Architecture: p2psync.Delegate handles peer state machine, INV exchange,
// and the catch-up sync flow (FetchBlockchainItemIDs → BlockchainItemIDsInventory
// → FetchItems → BlockMessage). Each accepted block fires
// chain.Database.SetOnBlockApplied which we intercept to (1) write to the
// block_log writer + (2) compare against --verify-against if set + (3)
// short-circuit at --target-block.
func (a *app) run() error {
	seeds := []string{}
	if !a.cfg.disableMainSeeds {
		seeds = append(seeds, network.MainnetSeeds...)
	}
	if a.cfg.extraSeeds != "" {
		seeds = append(seeds, a.cfg.extraSeeds)
	}
	a.logger.printf("starting (seeds=%d, listen=%q, target=%d)", len(seeds), a.cfg.listenEndpoint, a.cfg.targetBlock)

	// Wire the per-block hook BEFORE starting the sync delegate so the
	// first APPLIED already lands in the block_log.
	stopCh := make(chan struct{}, 1)
	a.chain.SetOnBlockApplied(func(blockNum uint32, _ block.ID) {
		raw := a.chain.RecentBlockBytes(blockNum)
		if raw == nil {
			a.logger.printf("WARN no raw bytes for applied block %d (cache miss)", blockNum)
		} else if err := a.afterApply(raw, blockNum); err != nil {
			a.logger.printf("ERROR post-apply hook block %d: %v", blockNum, err)
		}
		if a.cfg.targetBlock > 0 && blockNum >= a.cfg.targetBlock {
			select {
			case stopCh <- struct{}{}:
			default:
			}
		}
	})

	a.p2p = p2pnode.New(p2pnode.Config{
		Endpoint:           a.cfg.listenEndpoint,
		SeedNodes:          seeds,
		UserAgent:          "ghive-sync/0.1.0",
		MaxOutboundPeers:   a.cfg.maxOutboundPeers,
		MaxConcurrentDials: a.cfg.maxConcurrentDials,
	}, a.chain, nil)
	syncDel := p2psync.New(p2psync.Config{}, a.chain)
	a.p2p.SetDelegate(syncDel)
	if err := a.p2p.Start(); err != nil {
		return fmt.Errorf("p2p start: %w", err)
	}
	syncDel.Start()
	defer syncDel.Stop()
	defer a.p2p.Stop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		a.logger.printf("shutting down (signal)...")
	case <-stopCh:
		a.logger.printf("shutting down (target block %d reached)...", a.cfg.targetBlock)
	}
	return nil
}

// afterApply is invoked from chain.SetOnBlockApplied (i.e. after the
// block has already passed the full validator pipeline). We append it to
// the block_log writer + diff against --verify-against if set.
func (a *app) afterApply(rawBlock []byte, blockNum uint32) error {
	if a.verify != nil {
		want, err := a.verify.Block(blockNum)
		if err != nil {
			return fmt.Errorf("verify-against block %d: %w", blockNum, err)
		}
		if !bytesEqual(want, rawBlock) {
			return fmt.Errorf("verify-against mismatch at block %d (got %d bytes, ref %d bytes)",
				blockNum, len(rawBlock), len(want))
		}
	}
	if err := a.writer.Append(rawBlock); err != nil {
		return fmt.Errorf("append block %d: %w", blockNum, err)
	}
	a.logProgress(blockNum)
	return nil
}

// logProgress emits one "head=N (rate=X bps)" line whenever EITHER the
// block-step threshold (default 1000) OR the wall-clock interval
// (default 1s) since the last emission has passed. Reduces the per-block
// stderr-write overhead from O(blocks) to O(seconds), which buys ~10x
// on a fast catch-up where the previous one-line-per-block path was
// burning ~3% of CPU on stderr formatting.
func (a *app) logProgress(blockNum uint32) {
	if a.progressStartN.Load() == 0 {
		a.progressStartN.Store(blockNum)
	}
	now := time.Now().UnixNano()
	last := a.progressLastT.Load()
	lastN := a.progressLastN.Load()
	stepOK := a.progressBlocks > 0 && blockNum-lastN >= a.progressBlocks
	timeOK := a.progressInterval > 0 && time.Duration(now-last) >= a.progressInterval
	if !stepOK && !timeOK {
		return
	}
	if !a.progressLastT.CompareAndSwap(last, now) {
		return // another caller raced ahead; let them log
	}
	a.progressLastN.Store(blockNum)
	dt := float64(now-last) / float64(time.Second)
	rate := 0.0
	if dt > 0 {
		rate = float64(blockNum-lastN) / dt
	}
	overall := float64(blockNum-a.progressStartN.Load()) / time.Since(a.progressStartT).Seconds()
	a.logger.printf("head=%d  rate=%.0f bps  avg=%.0f bps", blockNum, rate, overall)
}

// applyBlock validates and persists one block. This is the hook the P2P
// sync delegate will call for every received block (once agent-sync lands).
//
// Validation is FULL: signature + merkle + every op evaluator. Replay
// optimizations (SkipBlockSignature, SkipTransactionSignatures) are NOT
// engaged here — live sync must verify the signatures from the wire.
func (a *app) applyBlock(rawBlock []byte, blockNum uint32) error {
	if a.verify != nil {
		want, err := a.verify.Block(blockNum)
		if err != nil {
			return fmt.Errorf("verify-against block %d: %w", blockNum, err)
		}
		if !bytesEqual(want, rawBlock) {
			return fmt.Errorf("verify-against mismatch at block %d (got %d bytes, ref %d bytes)",
				blockNum, len(rawBlock), len(want))
		}
	}

	if _, err := a.chain.ApplyBlock(rawBlock, blockNum, chain.ApplyOptions{
		SkipBlockSignature:        a.skipSigsForTest,
		SkipTransactionSignatures: a.skipSigsForTest,
		Sync:                      false,
	}, nil); err != nil {
		return fmt.Errorf("apply block %d: %w", blockNum, err)
	}

	if err := a.writer.Append(rawBlock); err != nil {
		return fmt.Errorf("append block %d: %w", blockNum, err)
	}

	if blockNum%1000 == 0 {
		hash := ""
		if hdrLen, ok := signedHeaderLen(rawBlock); ok {
			id := block.ConstructID(rawBlock[:hdrLen], blockNum)
			hash = hex.EncodeToString(id[:])
		}
		a.logger.printf("sync: applied block %d (hash=%s) -> head=%d", blockNum, hash, a.writer.HeadBlock())
	}

	if a.cfg.targetBlock > 0 && blockNum >= a.cfg.targetBlock {
		a.logger.printf("sync: reached target-block %d; closing writer and exiting", blockNum)
		// Close the writer (drains the index sidecar) before signaling
		// the run loop to exit. The cleanest way is to send ourselves a
		// SIGINT so the existing shutdown path runs.
		_ = a.writer.Close()
		a.writer = nil
		// Signal the main goroutine to exit. We can't return from inside
		// a delegate-driven callback, so SIGTERM is the simplest plumbing.
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
	return nil
}

// replayFromSource opens a source block_log, walks it in order, and feeds
// each block through applyBlock. Used by the unit test and as a smoke
// test before peers are wired.
func (a *app) replayFromSource(srcPath string, maxBlocks uint32) error {
	r, err := blocklog.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source %s: %w", srcPath, err)
	}
	defer r.Close()

	head := r.Head()
	if maxBlocks > 0 && maxBlocks < head {
		head = maxBlocks
	}
	if a.cfg.targetBlock > 0 && a.cfg.targetBlock < head {
		head = a.cfg.targetBlock
	}

	a.logger.printf("self-replay: source=%s head=%d", srcPath, head)
	startedAt := time.Now()
	for n := uint32(1); n <= head; n++ {
		raw, err := r.Block(n)
		if err != nil {
			return fmt.Errorf("read source block %d: %w", n, err)
		}
		if err := a.applyBlock(raw, n); err != nil {
			return err
		}
	}
	dt := time.Since(startedAt).Seconds()
	if dt > 0 {
		a.logger.printf("self-replay: applied %d blocks in %.2fs (%.0f blocks/s)", head, dt, float64(head)/dt)
	}
	return nil
}

// signedHeaderLen returns the length in bytes of the signed_block_header
// portion of raw, or (0,false) if it cannot be parsed.
func signedHeaderLen(raw []byte) (int, bool) {
	_, n, err := block.UnmarshalSignedHeader(raw)
	if err != nil {
		return 0, false
	}
	return n, true
}

// bytesEqual is a tiny helper to avoid importing "bytes" just for one call.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
