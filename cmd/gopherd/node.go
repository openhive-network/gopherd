// Command gopherd is the node: live-mainnet sync, block production and API.
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
// Compile: `make` (or `go build -o bin/gopherd ./cmd/gopherd`).
// Quick self-test: `go test ./cmd/gopherd/`.
// Live run (once sync agent lands): `ghive-sync --data-dir /tmp/sync-state`.
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"gopherd/internal/config"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gopherd/internal/api"
	"gopherd/internal/blocklog"
	"gopherd/internal/chain"
	"gopherd/internal/crypto"
	p2pnode "gopherd/internal/p2p/node"
	"gopherd/internal/plugins/witness"
	gosync "sync"

	p2psync "gopherd/internal/p2p/sync"
	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
	snap "gopherd/internal/state/snapshot"
)

// resumeMarkerInterval is how often (in blocks) an ephemeral run records its
// applied head, so a restart can rebuild to that point from the mirror. Small
// enough that a crash re-replays little; the write is a cheap atomic rename.
// Overridable via GHIVE_RESUME_MARKER_INTERVAL (used by tests).
var resumeMarkerInterval uint32 = 50000

// defaultMaxPeers mirrors hived's GRAPHENE_NET_DEFAULT_MAX_CONNECTIONS (200):
// "if we have maximum_number_of_connections or more, we will refuse any
// inbound connections".
const defaultMaxPeers = 200

func init() {
	if v := config.Env("GOPHERD_RESUME_MARKER_INTERVAL"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil && n > 0 {
			resumeMarkerInterval = uint32(n)
		}
	}
}

// resumeMarkerPath is where the ephemeral resume head is stored. It lives under
// blockchain/ so wipePebbleFiles (which preserves that subtree) doesn't delete
// it when we clean stale pebble state on resume.
func resumeMarkerPath(dataDir string) string {
	return filepath.Join(dataDir, "blockchain", "resume_head")
}

// readResumeMarker returns the last recorded ephemeral head, or 0 if none.
func readResumeMarker(dataDir string) uint32 {
	b, err := os.ReadFile(resumeMarkerPath(dataDir))
	if err != nil {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}

// writeResumeMarker atomically records the applied head for --recover-block-log
// resume. Best-effort: a failure just means the next restart replays from an
// older marker (or from 0).
func writeResumeMarker(dataDir string, head uint32) error {
	p := resumeMarkerPath(dataDir)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatUint(uint64(head), 10)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// apiEndpointIsLoopback reports whether a host:port bind address is restricted
// to the loopback interface. An empty/unspecified host (":8090", "0.0.0.0",
// "::") binds all interfaces and is treated as PUBLIC (not loopback); an
// unresolvable hostname is also treated as public (safe default — the caller
// uses this to decide whether to disable the DoS-prone unbounded-scan methods).
func apiEndpointIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // no port / unparseable — treat the whole string as the host
	}
	switch host {
	case "":
		return false // ":8090" binds all interfaces
	case "localhost":
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		return false
	}
	return ip.IsLoopback()
}

// peekPersistedHead reads the DGPO row from the state backend without
// constructing a chain.Database (so genesis.Apply hasn't reset it yet)
// and returns the persisted head block number. Returns 0 when the
// backend is empty or the row is not yet seeded.
func peekPersistedHead(db *state.Database) uint32 {
	s := state.NewSession(db)
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return 0
	}
	return dgpo.HeadBlockNumber
}

// wipeScratchColdTier removes a scratch cold tier (--cold-backend pebble or
// mmap) so a from-genesis recovery replay rebuilds it alongside the typed
// stores. A persist cold tier, or no directory, is left untouched. Reports
// whether anything was removed.
func wipeScratchColdTier(coldDir, coldBackend string) (bool, error) {
	if coldDir == "" || coldBackend == "persist" {
		return false, nil
	}
	if _, err := os.Stat(coldDir); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := os.RemoveAll(coldDir); err != nil {
		return false, err
	}
	return true, nil
}

// wipePebbleFiles removes pebble's state files from dir while preserving
// the blockchain/ subtree (block_log + index sidecar) and the peerdb.
// Used at startup when crash recovery has to rebuild state from genesis
// — pebble may hold partially-applied transactions (dedup table) from
// the previous run that would conflict with the replay. Once the typed-
// stores get pebble persistence (out of scope here), this can be a
// targeted "reset DGPO + dedup table only" operation instead.
func wipePebbleFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		name := e.Name()
		switch name {
		case "blockchain", "peerdb":
			// Preserve.
			continue
		}
		// Pebble files: *.log, *.sst, MANIFEST-*, CURRENT, OPTIONS-*,
		// LOCK, marker.* — we just nuke everything in the dir other
		// than the explicitly-preserved entries above.
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

func cmdNode(args []string) int {
	cfg := parseNodeFlags(args)
	if cfg.coldCacheMB > 0 {
		objects.ColdCacheBytes = int64(cfg.coldCacheMB) << 20
	}
	if cfg.memLimitGiB > 0 {
		debug.SetMemoryLimit(int64(cfg.memLimitGiB) << 30)
	}

	if cfg.showSeeds {
		for _, s := range network.MainnetSeeds {
			fmt.Println(s)
		}
		return 0
	}

	if cfg.dataDir == "" {
		fmt.Fprintln(os.Stderr, "gopherd node: --data-dir is required")
		return 2
	}

	if cfg.noBlockLog && cfg.sourceBlockLog != "" {
		fmt.Fprintln(os.Stderr, "gopherd node: --no-block-log is incompatible with --source-block-log (self-replay writes the local log)")
		return 2
	}
	if cfg.recoverBlockLog != "" && !cfg.noBlockLog {
		fmt.Fprintln(os.Stderr, "gopherd node: --recover-block-log requires --no-block-log (else the writer would target it)")
		return 2
	}
	// Supplying keys without a produce mode is refused rather than silently
	// ignored, so nobody can believe they configured a witness and get neither
	// production nor an error.
	if cfg.produceReal && !cfg.forkSwitching {
		// Producing without fork-choice is how the node wedged on 2026-08-10.
		// The linear apply path commits a block immediately, so our own block
		// becomes irreversible the moment we make it; when the network picks a
		// different block for that height, the competing block is filed stale,
		// its child buffers forever, and the node stops advancing at its own
		// orphan. Any block we produce may lose its slot race, so reversibility
		// is not optional for a producing node.
		fmt.Fprintln(os.Stderr, "gopherd node: --produce requires --fork-switching (a produced block must stay reversible so the node can reorg off it if the network chooses another block for that slot)")
		return 2
	}
	if cfg.produceReal && !cfg.p2pServe {
		// A passive node never advertises inventory nor serves blocks (see
		// Config.Serve), so a block it produces would be applied locally and
		// then never leave the box: the slot is missed with no error logged.
		fmt.Fprintln(os.Stderr, "gopherd node: --produce requires --p2p-serve (a passive node never advertises or serves blocks, so its produced blocks would never reach the network)")
		return 2
	}
	if cfg.produceReal && cfg.produceDryRun {
		fmt.Fprintln(os.Stderr, "gopherd node: --produce and --produce-dry-run are mutually exclusive")
		return 2
	}
	if cfg.witnessKeyFile != "" && !cfg.produceDryRun && !cfg.produceReal {
		fmt.Fprintln(os.Stderr, "gopherd node: --witness-keys requires --produce or --produce-dry-run")
		return 2
	}
	if (cfg.produceDryRun || cfg.produceReal) && cfg.witnessKeyFile == "" {
		fmt.Fprintln(os.Stderr, "gopherd node: block production requires --witness-keys (there is nothing to sign with otherwise)")
		return 2
	}
	if cfg.produceEverySlot && !cfg.produceDryRun {
		fmt.Fprintln(os.Stderr, "gopherd node: --produce-dry-run-every-slot requires --produce-dry-run")
		return 2
	}

	app, err := newApp(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gopherd node: %v\n", err)
		return 1
	}
	defer app.close()
	// pprof comes up before any replay: a replay from genesis runs for hours
	// and is the part most worth profiling.
	app.startPprof()

	if cfg.sourceBlockLog != "" {
		// Self-replay path. No peers, no sync — just stream blocks from
		// the source block_log into the validator + writer. This is what
		// the unit test in main_test.go exercises end-to-end.
		if err := app.replayFromSource(cfg.sourceBlockLog, cfg.maxBlocks); err != nil {
			fmt.Fprintf(os.Stderr, "gopherd node: replay: %v\n", err)
			return 1
		}
		return 0
	}

	// Crash recovery: bring the in-memory typed-stores back to the
	// persisted DGPO head by replaying the local block_log.
	if err := app.recover(); err != nil {
		fmt.Fprintf(os.Stderr, "gopherd node: %v\n", err)
		return 1
	}

	if err := app.run(); err != nil {
		fmt.Fprintf(os.Stderr, "gopherd node: %v\n", err)
		return 1
	}
	return 0
}

type flags struct {
	dataDir        string
	targetBlock    uint32
	verifyAgainst  string
	sourceBlockLog string
	coldDir        string
	coldBackend    string
	residentCap    int
	evictBudget    int
	memLimitGiB    int
	// coldCacheMB sizes the cold tier's pebble block cache (manual memory,
	// outside the Go heap and the soft memory limit).
	coldCacheMB int
	// emitVOps keeps chain.Database's virtual-op stream on. Nothing in the
	// node consumes it yet, so it defaults to off.
	emitVOps bool
	// persistStateDir keeps the typed stores in an mmap-backed persiststore so a
	// restart resumes at head. This is the structural answer to "why does hived
	// restart instantly and we don't": hived mmaps chainbase and is immediately
	// at head, whereas our typed stores live in the Go heap and are volatile, so
	// every start otherwise rebuilds them from a multi-GB snapshot.
	persistStateDir string
	// shutdownSnapshotDir, if set, saves a directory snapshot of the state at
	// the current head on clean exit / SIGTERM (block-<head>/), after flushing
	// the cold tier. Without it, `node` can LOAD a snapshot but never SAVE one,
	// so every restart rewinds to whenever --load-snapshot was taken and has to
	// re-sync the delta over P2P — a month-old snapshot costs ~2h of catch-up.
	// Mirrors replay's --shutdown-snapshot-dir.
	shutdownSnapshotDir string
	// witnessKeyFile / produceDryRun / produceEverySlot drive the block
	// production DRY RUN. Real production is intentionally NOT reachable from
	// this command: a produced block that got applied would fork this node off
	// the chain it is following, and one that got broadcast would be a witness
	// action taken by an unproven implementation.
	witnessKeyFile   string
	produceReal      bool
	produceDryRun    bool
	produceEverySlot bool
	// relayTransactions turns on transaction relay. Off by default: the first
	// implementation advertised the wrong identity and got us dropped 234 times.
	relayTransactions bool
	// loadSnapshot warm-starts the state from a directory snapshot
	// (block-N/ produced by ghive-tools replay --snapshot-every /
	// --save-dir-snapshot-at) BEFORE dialing peers. Use to skip the
	// from-genesis catch-up when iterating on a bug at a known block.
	loadSnapshot     string
	maxBlocks        uint32
	showSeeds        bool
	listenEndpoint   string
	extraSeeds       string
	initWitnesses    []string
	initWitnessesRaw string
	chainID          string
	disableMainSeeds bool
	// compressBlocks controls whether the on-disk block_log we write is
	// zstd-compressed (per-block, with the hived dictionary scheme) or
	// stored uncompressed. Off by default to keep existing tests +
	// downstream tooling that compares byte-identical against an
	// uncompressed reference unchanged.
	compressBlocks bool
	// noBlockLog runs the node WITHOUT writing a local block_log — state +
	// cold tier only. Turns a from-scratch p2p sync from a ~400 GB disk write
	// (a redundant copy of the chain) into tens of GB, for validation runs
	// where the local archive isn't wanted. Trade-off: no local block_log
	// means recover()-on-restart is impossible (typed-stores are volatile), so
	// a restart must warm-start from a snapshot or begin again from genesis.
	// Incompatible with --source-block-log (self-replay needs the writer).
	noBlockLog bool
	// recoverBlockLog is an EXTERNAL block_log used only to rebuild the
	// volatile typed-stores on restart (resume), decoupled from the writer.
	// Point it at the on-disk mirror so an ephemeral (--no-block-log) node can
	// resume after a crash: on startup, if pebble already has a persisted
	// head N>0, replay this log 1..N to rebuild the typed-stores, then
	// continue P2P from N — no self-written 400GB log required. Requires
	// --no-block-log (else the writer would target this path and corrupt it).
	recoverBlockLog string
	// backend selects the state backend. "pebble" (default, persistent)
	// or "mem" (in-memory btree — volatile, ~3-5x faster, suitable for
	// sync-verification runs that reset state on restart).
	backend string
	// cpuProfile, when non-empty, is the path to write a runtime/pprof
	// CPU profile to. The profile is started at app boot and stopped on
	// shutdown.
	cpuProfile string
	// memProfile, when non-empty, is the path to write a runtime/pprof
	// heap (allocation) profile to at shutdown. Captures alloc_space and
	// inuse_space samples.
	memProfile string
	// (--no-verify-merkle removed: the field was parsed but never read, and no
	// skip-merkle path exists anywhere in internal/chain, so the flag could not
	// have worked. It advertised "skip the per-block merkle-root
	// recomputation" while changing nothing.)
	// cacheMB sets the Pebble block cache in megabytes. Default 1024MB
	// (was 256MB hard-coded).
	cacheMB int
	// commitBatch sets how many blocks accumulate before pebble Commit
	// fires. 1 = legacy per-block; 100 = batched.
	commitBatch int
	// commitBatchFsync controls whether per-batch Commit fsyncs.
	commitBatchFsync bool
	// maxOutboundPeers caps active outbound peer count. 0 = p2pnode
	// default (32). Bigger value = more parallel block fetches.
	maxOutboundPeers int
	// maxPeers caps TOTAL connections. Without it the cap in
	// p2pnode.handleNewConn is inert (it is gated on MaxPeers > 0), so a
	// listening node accepted UNLIMITED inbound connections -- each one a file
	// descriptor, a pair of goroutines and a receive buffer. Any peer could
	// exhaust the process.
	maxPeers int
	// maxConcurrentDials caps in-flight dial attempts. 0 = p2pnode
	// default (24).
	maxConcurrentDials int
	// p2pServe enables ADVERTISING our inventory + SERVING blocks to peers.
	// DEFAULT false = passive fetch-only leecher (download only, never push).
	// A safety default: an unfinished/slow node that advertises but can't
	// serve promptly gets peers to drop it and can churn the network.
	p2pServe bool
	// logWorkTiming enables the periodic per-block apply "work" timing line.
	logWorkTiming bool
	// forkSwitching routes inbound blocks through the fork-choice engine
	// (chain.PushLiveBlock) so the node can ride the live head through depth-1
	// tip micro-forks. DEFAULT false = strictly-linear apply (replay/catch-up).
	forkSwitching bool
	// apiEndpoint, when set (host:port), serves the read-only JSON-RPC API over
	// HTTP. Empty = off. Bind to loopback (127.0.0.1:PORT) unless deliberately
	// exposing it; it is read-only and unrelated to the p2p port.
	apiEndpoint string

	// pprofAddr, when set, serves net/http/pprof. Needed because --memprofile
	// only writes at SHUTDOWN, which cannot show a TREND: diagnosing slow heap
	// growth requires two profiles taken while the process runs, diffed with
	// `go tool pprof -base`. Off by default; it exposes process internals.
	pprofAddr string
	// logAdaptiveBatch, when set, asks the sync delegate to log one
	// trace line per per-peer FetchBatch decision. Useful for tuning
	// the adaptive sizing curve; off by default to avoid noise.
	logAdaptiveBatch bool
}

func parseNodeFlags(args []string) flags {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	var f flags
	var target, max uint
	fs.StringVar(&f.dataDir, "data-dir", "", "data directory (state + blockchain/block_log live here)")
	fs.UintVar(&target, "target-block", 0, "stop sync after applying this block number (0 = unlimited)")
	fs.StringVar(&f.verifyAgainst, "verify-against", "", "path to a reference block_log; assert each applied block's bytes match")
	fs.StringVar(&f.sourceBlockLog, "source-block-log", "", "self-replay mode: read blocks from this block_log instead of P2P")
	fs.StringVar(&f.loadSnapshot, "load-snapshot", "", "warm-start from a directory snapshot (block-N/) before dialing peers; skips genesis catch-up")
	fs.BoolVar(&f.relayTransactions, "relay-transactions", false, "Relay transactions to peers (advertise by hived's legacy message hash + serve bodies on request). Without it the node only CONSUMES transactions, arriving at the end of every propagation chain — measured as holding 2 pending at build time where the producing witness held 28-33. Default off because the first implementation advertised the wrong identity and got us dropped 234 times; re-enable deliberately and watch for \"You sent me a message that I didn't request\".")
	fs.StringVar(&f.witnessKeyFile, "witness-keys", "", "file of `account WIF` lines (one per witness). Keys go in a file, not a flag, so they aren't exposed in ps output. Requires --produce or --produce-dry-run")
	fs.BoolVar(&f.produceReal, "produce", false, "REAL block production: when the schedule names one of our witnesses AND the chain's registered signing key for it matches the key we hold, build, sign, APPLY and BROADCAST the block. If the key does not match, log it and skip the slot (mirrors hived, which will not emit a block the network would reject). Requires --witness-keys; mutually exclusive with --produce-dry-run")
	fs.BoolVar(&f.produceDryRun, "produce-dry-run", false, "exercise the full block-production pipeline (schedule -> assemble -> merkle -> sign -> self-verify -> timing) and DISCARD every block. Never applies, never broadcasts. Use this to measure the pipeline without touching the network; --produce is the real mode")
	fs.BoolVar(&f.produceEverySlot, "produce-dry-run-every-slot", false, "with --produce-dry-run, build a block EVERY slot instead of only when the schedule names one of our witnesses. On mainnet we are never scheduled, so this is what produces measurable data; such blocks are built for our own account and logged as SIMULATED")
	fs.StringVar(&f.persistStateDir, "persist-state-dir", "", "ZERO-SNAPSHOT RESTART: keep all non-cold typed state in a persiststore container here — mmap-backed, like hived's chainbase shared_memory.bin. Saved on clean exit/SIGTERM. ⚠️ DOES NOT DELIVER A FAST RESTART AT MAINNET SCALE (measured 2026-08-10): on the pebble cold tier the load re-spills the entire ~25GB comment corpus, and a resume was still running at 22 MINUTES with RSS climbing to 25.6GiB — slower than the ~24min snapshot load it replaces. Only PersistCold implements durableColdTier (which would skip that re-spill), and building a persist cold tier showed ~10x write amplification (260GB written for a 25.6GB corpus) so it is not usable yet either. Leave this OFF unless you are working on exactly this. Takes precedence over --load-snapshot")
	fs.StringVar(&f.shutdownSnapshotDir, "shutdown-snapshot-dir", "", "on clean exit OR SIGINT/SIGTERM, save a dir snapshot here (block-<head>/) after flushing the cold tier — so a restart with --load-snapshot=<that> + the same --cold-dir resumes at head instead of re-syncing the delta over P2P")
	fs.UintVar(&max, "max-blocks", 0, "in self-replay mode, stop after this many blocks (0 = the source's head)")
	fs.BoolVar(&f.showSeeds, "show-seeds", false, "print the compiled-in mainnet seed list and exit")
	fs.StringVar(&f.listenEndpoint, "p2p-endpoint", "", "local listen address (host:port); empty = outbound-only")
	fs.Var((*repeatableSeeds)(&f.extraSeeds), "extra-seed", "additional seed (host:port); repeatable, and accepts a comma-separated list")
	fs.Var((*repeatableSeeds)(&f.initWitnessesRaw), "init-witness", "TESTNET ONLY: extra genesis witness registered with the shared init key, mirroring hived's init_witnesses. Repeatable / comma-separated. Must match the hived nodes on the same testnet exactly, or genesis state differs and the chains disagree at block 1")
	fs.StringVar(&f.chainID, "chain-id", "", "override the chain ID (64-hex), mirroring hived's --chain-id. Empty = the build's default. Wrong value = the P2P handshake never completes")
	fs.BoolVar(&f.disableMainSeeds, "no-default-seeds", false, "skip the compiled-in mainnet seeds; only --extra-seed peers will be dialed")
	fs.BoolVar(&f.compressBlocks, "compress-blocks", false, "zstd-compress each block in the local block_log (matches hived's on-disk format byte-for-byte)")
	fs.BoolVar(&f.noBlockLog, "no-block-log", false, "do NOT write a local block_log (state + cold tier only); shrinks a from-scratch p2p sync from ~400GB to tens of GB. No local log = no recover-on-restart. Incompatible with --source-block-log")
	fs.StringVar(&f.recoverBlockLog, "recover-block-log", "", "external block_log used ONLY to rebuild typed-stores on restart (resume). Point at the on-disk mirror so a --no-block-log node resumes from its persisted pebble head instead of re-syncing from 0. Requires --no-block-log")
	fs.StringVar(&f.backend, "backend", "", "state backend: pebble (persistent) or mem (volatile, ~3-5x faster); default = pebble unless --verify-against is set, in which case mem (since the run is throwaway anyway)")
	fs.StringVar(&f.cpuProfile, "cpuprofile", "", "write runtime/pprof CPU profile to this path")
	fs.StringVar(&f.memProfile, "memprofile", "", "write runtime/pprof heap profile to this path at shutdown")
	fs.IntVar(&f.cacheMB, "cache-mb", 1024, "pebble block-cache size in MB (default 1024)")
	fs.IntVar(&f.commitBatch, "commit-batch", 100, "blocks per pebble Apply (1 = legacy per-block; >1 batches)")
	fs.BoolVar(&f.commitBatchFsync, "commit-batch-fsync", true, "fsync each batch boundary (default true)")
	fs.IntVar(&f.maxOutboundPeers, "max-outbound-peers", 0, "max active outbound P2P peers (0 = default 32)")
	fs.IntVar(&f.maxPeers, "max-peers", defaultMaxPeers, "max TOTAL P2P connections (inbound + outbound); inbound is refused at the cap. 0 disables the cap, which leaves the node open to connection-exhaustion")
	fs.IntVar(&f.maxConcurrentDials, "max-concurrent-dials", 0, "max in-flight dial attempts (0 = default 24)")
	fs.BoolVar(&f.logAdaptiveBatch, "log-adaptive-batch", false, "log one trace per per-peer FetchBatch decision (peer X batch=N rate=Y bps)")
	fs.BoolVar(&f.p2pServe, "p2p-serve", false, "advertise inventory + serve blocks to peers (default false = passive fetch-only; enable ONLY for a known-correct node that keeps up in real time)")
	fs.BoolVar(&f.logWorkTiming, "log-work-timing", false, "log periodic per-block apply work-timing (avg/p50/p99/max ms) — comparable to hived's block-processing timing")
	fs.BoolVar(&f.forkSwitching, "fork-switching", false, "route inbound blocks through the fork-choice engine (depth-1 tip reorg) instead of strictly-linear apply (default off)")
	fs.StringVar(&f.pprofAddr, "pprof-addr", "", "serve net/http/pprof at host:port (e.g. 127.0.0.1:6060) for live heap/CPU profiling; empty = off")
	fs.StringVar(&f.apiEndpoint, "api-endpoint", "", "serve the read-only JSON-RPC API over HTTP at host:port (e.g. 127.0.0.1:8090); empty = off")
	fs.IntVar(&f.residentCap, "comment-resident-cap", 0, "if >0, cap RAM-resident comments to this many; frozen comments + votes spill to an on-disk cold tier (required for a full-chain node — otherwise the whole comment corpus is resident and OOMs)")
	fs.StringVar(&f.coldDir, "cold-dir", "", "directory for the cold tier (used when --comment-resident-cap>0). With --cold-backend=persist it must be stable across restarts")
	fs.StringVar(&f.coldBackend, "cold-backend", "", "cold-tier backend: \"\"/mmap|pebble|persist")
	fs.IntVar(&f.evictBudget, "evict-budget", 20000, "max comments evicted per block when over the resident cap")
	fs.BoolVar(&f.emitVOps, "emit-vops", false, "build the per-block virtual-operation stream while applying blocks (chain.LastBlockVOps). No node API serves it yet, so it is off by default; it costs allocation on the apply path")
	fs.IntVar(&f.coldCacheMB, "cold-cache-mb", 1024, "pebble block-cache size in MB for the cold tier (--cold-backend=pebble). This is manually managed memory OUTSIDE the Go heap, so it does not count against --mem-limit-gib; together with --cache-mb and the memtables it is what sits between the Go limit and a cgroup cap")
	fs.IntVar(&f.memLimitGiB, "mem-limit-gib", 0, "if >0, set the Go soft memory limit (debug.SetMemoryLimit) to this many GiB. NOTE: raising this SLOWS SNAPSHOT LOADING — the heap competes with the OS page cache over a ~26 GB snapshot file. Measured on a 31 GB box, same snapshot: 22 GiB -> 155s (166 MB/s), 12 GiB -> 108s (238 MB/s), 12 GiB with the file already cached -> 65s (394 MB/s). Leave headroom for the page cache rather than handing it all to Go")
	fs.Parse(args)
	f.targetBlock = uint32(target)
	f.maxBlocks = uint32(max)
	for _, w := range strings.Split(f.initWitnessesRaw, ",") {
		if w = strings.TrimSpace(w); w != "" {
			f.initWitnesses = append(f.initWitnesses, w)
		}
	}
	// Apply the chain-ID override BEFORE anything signs or handshakes.
	if f.chainID != "" {
		h, err := hex.DecodeString(f.chainID)
		if err != nil || len(h) != 32 {
			fmt.Fprintf(os.Stderr, "gopherd node: --chain-id must be 64 hex chars, got %q\n", f.chainID)
			os.Exit(2)
		}
		var id network.Hash32
		copy(id[:], h)
		applyChainIDOverride(id)
		fmt.Fprintf(os.Stderr, "gopherd node: chain ID overridden to %s\n", id.Hex())
	}
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
	// fatalCh is closed when the node must stop applying blocks because state
	// is no longer trustworthy (currently: a lost cold-tier write). Fail-stop
	// rather than fail-silent — see commentStore.ColdWriteErr.
	fatalCh   chan struct{}
	fatalOnce sync.Once

	cfg          flags
	state        *state.Database
	chain        *chain.Database
	writer       blocklog.Writer
	verify       *blocklog.Reader // optional, for --verify-against
	p2p          *p2pnode.Node
	logger       *appLogger
	blockLogPath string
	// recoverFrom > 0 when the daemon detected a non-empty pebble or
	// non-empty blocklog at startup; recover() will replay blocks
	// 1..recoverFrom against the local blocklog before opening the
	// p2p subsystem to bring the in-memory typed-stores back in sync
	// with the persisted DGPO. 0 = cold start, no recovery needed.
	recoverFrom uint32
	// recoverStart is the first block recover() applies: 1 for a cold
	// rebuild, snapshotHead+1 when the state was loaded from a snapshot.
	// Replaying from genesis onto a loaded snapshot applied block 1's state
	// over block 26.8M's and failed at block 1092 (then wrote a "block-1091"
	// snapshot of the mess).
	recoverStart uint32

	// progress tracking — afterApply rate-limits its log line to one
	// emission per progressInterval (or per progressBlocks blocks).
	progressBlocks   uint32
	progressInterval time.Duration
	progressLastT    atomic.Int64
	progressLastN    atomic.Uint32
	progressStartT   time.Time
	progressStartN   atomic.Uint32

	// blocksStoredAsReceived / blocksRecompressed count block_log appends
	// that reused the peer's compressed payload vs. compressed the raw block
	// locally (only the second costs apply-loop CPU).
	blocksStoredAsReceived atomic.Uint64
	blocksRecompressed     atomic.Uint64

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

	// Cold tier (only when --comment-resident-cap>0): frozen comments +
	// their votes spill to disk so a full-chain node's RAM stays bounded.
	// EvictFrozen runs after each applied block. Nil when off.
	cold         objects.ColdStore
	witness      *witness.Plugin
	commentStore *objects.CommentStore
	voteStore    *objects.CommentVoteStore
	evictBudget  int
}

func newApp(cfg flags) (*app, error) {
	// Crash-recovery preflight: if the local block_log has any blocks
	// AND we're using pebble (i.e. there's persisted state to clean up),
	// wipe the pebble directory so we don't end up double-applying
	// transactions that already landed in the previous run's dedup
	// table. The block_log is preserved (writer reopens at HEAD); the
	// in-memory typed-stores get rebuilt by recover() replaying from
	// genesis. This is the only way to avoid stale-K/V corruption while
	// typed-stores remain volatile across restarts.
	blockLogPath := filepath.Join(cfg.dataDir, "blockchain", "block_log")
	if cfg.backend != "mem" {
		if _, err := os.Stat(blockLogPath); err == nil {
			if err := wipePebbleFiles(cfg.dataDir); err != nil {
				return nil, fmt.Errorf("wipe stale pebble at %s: %w", cfg.dataDir, err)
			}
			// The cold tier is state too. It holds the comments and votes
			// evicted by the previous run, i.e. their shape at the OLD head;
			// a replay from genesis that promotes them back sees paid-out,
			// later-voted comments while applying earlier blocks, and pays
			// those cashouts wrong. Found on a mainnet sync: after a restart
			// the recovery replay credited a-alice 0 HBD for a comment hived
			// paid 0.019 HBD, and the next claim_reward_balance failed.
			// Scratch cold backends (pebble, mmap) are rebuilt by the same
			// replay; a persist cold tier is meant to survive restarts and
			// is left alone.
			// Not when a snapshot or persist-state is being loaded: those
			// resume at head and need the cold tier that was flushed with them.
			if cfg.loadSnapshot != "" || cfg.persistStateDir != "" {
				// keep
			} else if wiped, err := wipeScratchColdTier(cfg.coldDir, cfg.coldBackend); err != nil {
				return nil, fmt.Errorf("wipe stale cold tier at %s: %w", cfg.coldDir, err)
			} else if wiped {
				fmt.Fprintf(os.Stderr, "gopherd node: recovery from block_log — wiped stale cold tier %s (rebuilt by the replay)\n", cfg.coldDir)
			}
		}
	}

	// Ephemeral resume (--recover-block-log + a resume marker from a prior run):
	// rebuild deterministically from the mirror up to the marked head. Wipe
	// stale pebble K/V (a hard crash may have left state PAST the marker that a
	// replay-to-marker wouldn't overwrite) and the cold tier, so the rebuild is
	// clean. The marker lives under blockchain/, which wipePebbleFiles preserves.
	resumeHead := uint32(0)
	if cfg.recoverBlockLog != "" && cfg.backend != "mem" {
		resumeHead = readResumeMarker(cfg.dataDir)
		if resumeHead > 0 {
			if err := wipePebbleFiles(cfg.dataDir); err != nil {
				return nil, fmt.Errorf("resume: wipe stale pebble: %w", err)
			}
			if cfg.coldDir != "" {
				if err := os.RemoveAll(cfg.coldDir); err != nil {
					return nil, fmt.Errorf("resume: wipe stale cold dir: %w", err)
				}
			}
		}
	}

	var stateDB *state.Database
	switch cfg.backend {
	case "mem":
		stateDB = state.OpenMem()
	case "", "pebble":
		var err error
		cacheBytes := int64(cfg.cacheMB) << 20
		if cacheBytes <= 0 {
			cacheBytes = 1024 << 20
		}
		stateDB, err = state.Open(cfg.dataDir, cacheBytes)
		if err != nil {
			return nil, fmt.Errorf("open state %s: %w", cfg.dataDir, err)
		}
	default:
		return nil, fmt.Errorf("unknown --backend %q (want pebble or mem)", cfg.backend)
	}
	// After wipe, peek is always 0 — but keep the call so the recovery
	// path also works for callers that wire it differently.
	persistedHead := peekPersistedHead(stateDB)
	// --load-snapshot warm-starts state from a directory snapshot (the
	// dir form produced by ghive-tools replay --snapshot-every).
	// Registers all typed subsystems first so the loader's subsystem
	// set matches the producing process. Skips genesis.Apply because
	// the snapshot already carries the bootstrapped state.
	loadedFromSnapshot := false
	// snapshotHead is the block a --load-snapshot state is at; recovery from
	// the local block_log then starts at snapshotHead+1, not at genesis.
	snapshotHead := uint32(0)
	var cold objects.ColdStore
	var commentStore *objects.CommentStore
	var voteStore *objects.CommentVoteStore
	// ZERO-SNAPSHOT RESTART. If a persiststore container exists at
	// --persist-state-dir, restore from it and skip both genesis and the
	// snapshot load entirely: the typed stores come back mmap-backed at head,
	// the way hived resumes from chainbase. Falls through to the normal paths
	// when the container is absent (first ever run), so the flag can be set
	// permanently and simply starts working after the first clean shutdown.
	if cfg.persistStateDir != "" {
		psHead, psOK := snap.PersistStateHead(cfg.persistStateDir)
		// A persist-state resume restores the non-cold typed state from the
		// container but keeps using the EXISTING cold dir for comments/votes.
		// Those two must come from the same clean shutdown. Verify it: an
		// unverifiable pair is refused and we fall back to the (self-contained)
		// snapshot path rather than loading state we cannot vouch for. Hive
		// blocks carry no state root, so a mismatch here would never surface as
		// an error — the node would simply be wrong.
		if psOK {
			if coldHead, cOK := readColdHeadStamp(cfg.coldDir); !cOK || coldHead != psHead {
				got := "absent"
				if cOK {
					got = fmt.Sprintf("%d", coldHead)
				}
				fmt.Fprintf(os.Stderr, "gopherd node: REFUSING persist-state resume — container is at block %d but the cold tier stamp is %s. "+
					"The two must come from the same clean shutdown; falling back to the snapshot path.\n", psHead, got)
				psOK = false
			}
		}
		if psOK {
			boot := state.NewSession(stateDB)
			registerTypedSubsystems(boot)
			var cerr error
			cold, commentStore, voteStore, cerr = attachColdTier(stateDB, boot, cfg)
			if cerr != nil {
				stateDB.Close()
				return nil, cerr
			}
			h, lerr := snap.LoadPersist(stateDB, cfg.persistStateDir)
			if lerr != nil {
				stateDB.Close()
				return nil, fmt.Errorf("load persist-state %s: %w", cfg.persistStateDir, lerr)
			}
			fmt.Fprintf(os.Stderr, "gopherd node: RESUMED from persist-state %s at block %d — no snapshot load\n",
				cfg.persistStateDir, h)
			loadedFromSnapshot = true
		} else {
			fmt.Fprintf(os.Stderr, "gopherd node: persist-state %s is empty — starting normally; it will be written on clean shutdown\n",
				cfg.persistStateDir)
		}
	}
	if !loadedFromSnapshot && cfg.loadSnapshot != "" {
		boot := state.NewSession(stateDB)
		// Register EVERY typed subsystem the snapshot may carry — the
		// loader fails if the snapshot has a subsystem the DB hasn't
		// registered. Must match the replay-side set exactly.
		objects.EnsureWitnessStore(boot)
		objects.EnsureWitnessVoteStore(boot)
		objects.EnsureAccountStore(boot)
		cs := objects.EnsureCommentStore(boot)
		cvs := objects.EnsureCommentVoteStore(boot)
		objects.EnsureBlockSummaryStore(boot)
		objects.EnsureWitnessScheduleStore(boot)
		objects.EnsureFutureWitnessScheduleStore(boot)
		objects.EnsureDGPOStore(boot)
		objects.EnsureCommentCashoutScheduleStore(boot)
		objects.EnsureLimitOrderStore(boot)
		objects.EnsureVestingWithdrawalScheduleStore(boot)
		objects.EnsureTransactionStore(boot)
		objects.EnsureConvertRequestStore(boot)
		// Make the GHIVE_* switches visible BEFORE the load: several of them
		// change how state is loaded (GHIVE_COLD_REUSE will serve stale rows
		// against a mismatched cold dir), none appear in --help, and the load
		// takes over a minute — an operator should see the active set first.
		logActiveEnvSwitches(func(f string, v ...any) { fmt.Fprintf(os.Stderr, "gopherd node: "+f+"\n", v...) })
		// Attach the cold tier BEFORE loading so the multi-GB comment +
		// vote corpus spills to disk during the load instead of piling
		// up resident (a full-chain snapshot loaded all-resident OOMs).
		if cfg.residentCap > 0 {
			backendName := cfg.coldBackend
			if backendName == "persist" && cfg.coldDir == "" {
				stateDB.Close()
				return nil, fmt.Errorf("--cold-backend=persist requires a stable --cold-dir")
			}
			c, cerr := objects.OpenColdTierBackend(cfg.coldDir, backendName)
			if cerr != nil {
				stateDB.Close()
				return nil, fmt.Errorf("open cold tier: %w", cerr)
			}
			cold = c
			cs.SetColdTier(cold)
			cs.SetResidentCap(cfg.residentCap)
			cvs.SetColdTier(cold)
			commentStore = cs
			voteStore = cvs
			fmt.Fprintf(os.Stderr, "gopherd node: cold tier ON — resident cap %d, evict budget %d/block, dir %s\n",
				cfg.residentCap, cfg.evictBudget, cold.Dir())
		}
		// COLD SIDECAR: with GHIVE_COLD_REUSE the load no longer REWRITES the
		// cold rows, but it still streams all 26 GB to rebuild the bloom and
		// coldTotal. Both are pure functions of a cold tier that is already on
		// disk and unchanged, so restore them from a sidecar instead and let
		// spillToColdReuse skip the per-record work.
		//
		// Only valid for the exact (tier, snapshot) pair that wrote it; the
		// sidecar carries the block number and a CRC and refuses on mismatch,
		// because a stale bloom yields false NEGATIVES (a cold comment reads as
		// missing) rather than a loud failure.
		sidecarUsed := false
		// Ask the store rather than re-reading the env: comment_store latches
		// GHIVE_COLD_REUSE at package init, so a second os.Getenv here could
		// disagree with it (e.g. a test setting the var after init). The two
		// halves must agree — sidecar-loaded bookkeeping is only correct if the
		// per-record rebuild is actually being skipped.
		if cfg.residentCap > 0 && cfg.coldDir != "" && objects.ColdReuseEnabled() {
			if bn, berr := snapshotManifestBlock(cfg.loadSnapshot); berr == nil {
				ok, serr := commentStore.LoadColdSidecar(cfg.coldDir, bn)
				if serr != nil {
					fmt.Fprintf(os.Stderr, "gopherd node: cold sidecar rejected (%v) — falling back to full scan\n", serr)
				}
				if ok {
					commentStore.SetSidecarLoaded(true)
					sidecarUsed = true
					fmt.Fprintf(os.Stderr, "gopherd node: cold sidecar loaded (block %d, %d cold rows) — skipping bloom rebuild\n",
						bn, commentStore.ColdTotal())
				}
			}
		}
		// Past HF17 the by-root cold index is never read (SetColdByRootEnabled);
		// left on, the load writes every cold comment twice and keeps a
		// per-thread count map of several GB.
		if commentStore != nil {
			if bn, berr := snapshotManifestBlock(cfg.loadSnapshot); berr == nil && hardfork.SetForBlock(bn+1).Has(hardfork.HF17) {
				commentStore.SetColdByRootEnabled(false)
			}
		}
		fmt.Fprintf(os.Stderr, "gopherd node: loading snapshot from %s (%s)...\n",
			cfg.loadSnapshot, humanDirSize(cfg.loadSnapshot))
		man, err := snap.Load(stateDB, cfg.loadSnapshot)
		if err != nil {
			stateDB.Close()
			return nil, fmt.Errorf("load-snapshot %s: %w", cfg.loadSnapshot, err)
		}
		if cfg.residentCap > 0 && cfg.coldDir != "" && !sidecarUsed && commentStore != nil {
			// First run against this tier: bank the bookkeeping so the NEXT
			// start can skip the scan.
			if serr := commentStore.SaveColdSidecar(cfg.coldDir, man.BlockNum); serr != nil {
				fmt.Fprintf(os.Stderr, "gopherd node: cold sidecar save failed (non-fatal): %v\n", serr)
			} else {
				fmt.Fprintf(os.Stderr, "gopherd node: cold sidecar written for block %d\n", man.BlockNum)
			}
		}
		fmt.Fprintf(os.Stderr, "gopherd node: snapshot loaded — block %d, %s total\n",
			man.BlockNum, snap.HumanDuration(time.Since(procStart)))
		loadedFromSnapshot = true
		snapshotHead = man.BlockNum
	}
	if !loadedFromSnapshot {
		if err := genesis.ApplyForNetwork(stateDB, cfg.initWitnesses); err != nil {
			// Apply tolerates an already-bootstrapped dir.
			_ = err
		}
		// Wire the cold tier for the FROM-0 path too. The snapshot branch above
		// attaches it before loading; without the same wiring here, a
		// from-scratch P2P sync keeps EVERY comment resident (commentStore stays
		// nil so afterApply's EvictFrozen is a no-op) and GC-spirals into the
		// mem-limit in the dense era (~20M) — throughput collapses to ~100 bps.
		if cfg.residentCap > 0 {
			boot := state.NewSession(stateDB)
			cs := objects.EnsureCommentStore(boot)
			cvs := objects.EnsureCommentVoteStore(boot)
			backendName := cfg.coldBackend
			if backendName == "persist" && cfg.coldDir == "" {
				stateDB.Close()
				return nil, fmt.Errorf("--cold-backend=persist requires a stable --cold-dir")
			}
			c, cerr := objects.OpenColdTierBackend(cfg.coldDir, backendName)
			if cerr != nil {
				stateDB.Close()
				return nil, fmt.Errorf("open cold tier: %w", cerr)
			}
			cold = c
			cs.SetColdTier(cold)
			cs.SetResidentCap(cfg.residentCap)
			cvs.SetColdTier(cold)
			commentStore = cs
			voteStore = cvs
			fmt.Fprintf(os.Stderr, "gopherd node: cold tier ON — resident cap %d, evict budget %d/block, dir %s\n",
				cfg.residentCap, cfg.evictBudget, cold.Dir())
		}
	}
	cdb := chain.New(stateDB)
	// The per-block virtual-op stream is on by default in chain.Database, but
	// nothing in the node reads it (no API exposes it yet); building it was
	// ~7% of all bytes allocated during a sync. Off unless asked for.
	cdb.SetVOpEmission(cfg.emitVOps)
	// Enable the recent-block cache so afterApply can pull each freshly-
	// applied block's raw bytes back out and stream them into the writer.
	// Without this, RecentBlockBytes returns nil and our block_log stays
	// empty even though every block validates.
	cdb.EnableRecentBlockCache(2048)
	// Configure batched commits when running pebble — hides the per-block
	// fsync cost behind a single batch fsync. Mem backend is already
	// volatile so batching adds no value (fsync is a no-op on memBackend).
	if cfg.commitBatch > 1 && cfg.backend != "mem" {
		cdb.SetCommitBatch(uint32(cfg.commitBatch), cfg.commitBatchFsync)
	}

	blockchainDir := filepath.Join(cfg.dataDir, "blockchain")
	if err := os.MkdirAll(blockchainDir, 0o755); err != nil {
		stateDB.Close()
		return nil, fmt.Errorf("mkdir blockchain: %w", err)
	}
	// Ephemeral (--no-block-log): skip the writer entirely. State + cold tier
	// only; afterApply's append is guarded on writer!=nil, and recoverFrom
	// stays 0 (no local log to replay from on a restart).
	var w blocklog.Writer
	if !cfg.noBlockLog {
		var werr error
		w, werr = blocklog.OpenForWrite(blockLogPath, cfg.compressBlocks)
		if werr != nil {
			stateDB.Close()
			return nil, fmt.Errorf("open block_log writer: %w", werr)
		}
	}

	// Crash recovery: if the local block_log has blocks (writer reopened
	// at HEAD>0) AND pebble previously had DGPO at some N, replay from
	// block 1 (typed-stores are volatile and need rebuilding from genesis)
	// to whichever of writer-head/persistedHead is higher. Replay uses
	// the same fast batched-commit + skip-sigs settings as a fresh sync.
	//
	// Why replay from 1 instead of from persistedHead+1: the typed
	// stores (Account, Witness, Comment, CommentVote, WitnessVote) are
	// in-memory only and were just re-seeded by genesis.Apply. To bring
	// them up to N requires applying every block from 1..N. The pebble
	// K/V state from a previous run is overwritten as the replay
	// proceeds, so the persistedHead is only relevant as a sanity hint
	// (we expect blocklog.HeadBlock >= persistedHead).
	recoverFrom := uint32(0)
	if w != nil && (w.HeadBlock() > 0 || persistedHead > 0) {
		recoverFrom = w.HeadBlock()
		if persistedHead > recoverFrom {
			recoverFrom = persistedHead
		}
	}
	// Resume (--no-block-log + --recover-block-log): the typed-stores AND the
	// pebble K/V were just wiped/started-fresh, so rebuild everything by
	// replaying the EXTERNAL mirror log 1..resumeHead (the marker recorded by
	// the prior run), then P2P continues from there. recover() reads
	// a.blockLogPath, so point it at the mirror.
	if cfg.recoverBlockLog != "" && resumeHead > 0 {
		blockLogPath = cfg.recoverBlockLog
		recoverFrom = resumeHead
	}

	a := &app{
		fatalCh:          make(chan struct{}),
		cfg:              cfg,
		state:            stateDB,
		chain:            cdb,
		writer:           w,
		blockLogPath:     blockLogPath,
		recoverFrom:      recoverFrom,
		recoverStart:     snapshotHead + 1,
		logger:           newAppLogger(os.Stderr),
		progressBlocks:   1000,
		progressInterval: time.Second,
		progressStartT:   time.Now(),
		cold:             cold,
		commentStore:     commentStore,
		voteStore:        voteStore,
		evictBudget:      cfg.evictBudget,
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

// fatalf records an unrecoverable state condition: log it loudly and trigger an
// orderly shutdown. Used instead of panic so the node still flushes and writes
// its shutdown snapshot rather than aborting mid-operation.
func (a *app) fatalf(format string, args ...any) {
	a.fatalOnce.Do(func() {
		a.logger.printf("FATAL: "+format, args...)
		if a.fatalCh != nil {
			close(a.fatalCh)
		}
	})
}

func (a *app) close() {
	// Heap profile is written BEFORE we tear anything else down so the
	// snapshot reflects what was live at peak, not after Close has freed
	// the writer/state buffers.
	if a.cfg.memProfile != "" {
		if f, err := os.Create(a.cfg.memProfile); err == nil {
			runtime.GC()
			if werr := pprof.WriteHeapProfile(f); werr != nil {
				a.logger.printf("WARN write heap profile: %v", werr)
			} else {
				a.logger.printf("heap profile -> %s", a.cfg.memProfile)
			}
			_ = f.Close()
		} else {
			a.logger.printf("WARN create memprofile %s: %v", a.cfg.memProfile, err)
		}
	}
	// Stop the dry-run producer before the rest: its loop reads chain state
	// each slot, and it must not race the teardown below.
	if a.witness != nil {
		a.witness.Stop()
		st := a.witness.DryRunSnapshot()
		avg := 0.0
		if st.Built > 0 {
			avg = float64(st.TotalNs) / float64(st.Built) / 1e6
		}
		a.logger.printf("block production DRY RUN summary: slots=%d built=%d ours=%d verify_ok=%d verify_fail=%d errors=%d avg=%.2fms max=%.2fms (budget %ds)",
			st.Slots, st.Built, st.Ours, st.VerifyOK, st.VerifyFail, st.Errors,
			avg, float64(st.MaxNs)/1e6, int(witness.BlockInterval/time.Second))
		a.witness = nil
	}
	if a.p2p != nil {
		a.p2p.Stop()
		a.p2p = nil
	}
	// Flush any partial batched-commit before closing pebble — otherwise
	// up to commitBatch blocks of K/V state would be lost. The block_log
	// is independent so the bytes survive regardless, but the persisted
	// DGPO head would be a batch-boundary behind on next start, forcing
	// the crash-recovery replay to redo work.
	//
	// Ordered BEFORE the shutdown snapshot: P2P is stopped (head is stable)
	// and the K/V is consistent, so the snapshot captures a coherent state.
	if a.chain != nil && a.chain.CommitBatchSize() > 1 {
		if err := a.chain.FlushBatch(true); err != nil {
			a.logger.printf("WARN final FlushBatch: %v", err)
		}
	}
	// Shutdown snapshot FIRST — must run while the cold tier is still OPEN,
	// since a self-contained snapshot serializes cold-resident comments/votes by
	// reading them back out of it.
	//
	// Ordered ahead of the persist-state save deliberately. The persist save
	// once OOM-killed the process mid-shutdown, and because it ran first, the
	// snapshot never got its turn either — one bug cost BOTH restart artifacts.
	// The snapshot is the slower path but the proven one, so it goes first and
	// a failure in the newer path can only cost the fast option, never the
	// fallback.
	a.saveShutdownSnapshot()
	// Then the typed stores, so the next start resumes at head in seconds
	// instead of rebuilding from that snapshot.
	persistHead := a.savePersistState()
	if a.cold != nil {
		_ = a.cold.Close()
		a.cold = nil
		// Stamp the cold tier with the head persist-state was written at, but
		// only AFTER Close() has flushed it. A persist-state resume is a PAIR —
		// the container holds the non-cold typed state, the cold dir holds
		// comments/votes — and nothing else ties them together. Without this
		// stamp a mismatched pair loads silently, and Hive blocks carry no
		// state root, so the divergence would never surface as an error: the
		// node would just be quietly wrong while signing blocks.
		if persistHead != 0 {
			if werr := writeColdHeadStamp(a.cfg.coldDir, persistHead); werr != nil {
				a.logger.printf("WARN cold head stamp at %d: %v (next start will refuse the fast resume)", persistHead, werr)
			}
		}
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

// savePersistState writes the typed stores into the persiststore container so
// the next start resumes AT HEAD in seconds, the way hived resumes from
// chainbase, instead of rebuilding them from a multi-GB snapshot (~20 min).
//
// Must be called with the cold tier still open (see close()). Failures are
// logged, never fatal: losing this costs time on the next start, whereas
// aborting shutdown over it risks losing more.
func (a *app) savePersistState() uint32 {
	if a.cfg.persistStateDir == "" || a.state == nil || a.chain == nil {
		return 0
	}
	head, err := a.chain.HeadBlock()
	if err != nil || head == 0 {
		a.logger.printf("WARN persist-state: no head to save (err=%v head=%d)", err, head)
		return 0
	}
	start := time.Now()
	if serr := snap.SavePersist(a.state, a.cfg.persistStateDir, head); serr != nil {
		a.logger.printf("WARN persist-state save at %d failed: %v", head, serr)
		return 0
	}
	a.logger.printf("persist-state saved at block %d in %.1fs — restart with the same --persist-state-dir resumes here without a snapshot load",
		head, time.Since(start).Seconds())
	return head
}

// saveShutdownSnapshot writes a directory snapshot of the current head so the
// next start can warm-start from it instead of re-syncing over P2P.
//
// Why this exists: `node` could LOAD a snapshot but never SAVE one, so every
// restart rewound to whenever the --load-snapshot was produced. Once that
// snapshot is a month old, a restart costs the full delta over P2P (~900K
// blocks, ~2h) purely to get back to where the process already was. The typed
// stores are in-memory and pebble's DGPO is not durably anchored, so there is
// no cheaper resume path — the snapshot IS the anchor.
//
// Must be called with the cold tier still open (see close()). Failures are
// logged, never fatal: a missing snapshot costs time on the next start, but
// aborting shutdown over it would be worse.
func (a *app) saveShutdownSnapshot() {
	if a.cfg.shutdownSnapshotDir == "" || a.state == nil || a.chain == nil {
		return
	}
	head, err := a.chain.HeadBlock()
	if err != nil || head == 0 {
		a.logger.printf("WARN shutdown snapshot: no head to save (err=%v head=%d)", err, head)
		return
	}
	// Flush the cold tier first so its on-disk contents match the snapshot —
	// a restart pairs --load-snapshot with the same --cold-dir.
	if cc, ok := a.cold.(objects.CompactableCold); ok && cc != nil {
		if ferr := cc.Flush(); ferr != nil {
			a.logger.printf("WARN shutdown snapshot: cold flush: %v", ferr)
		}
	}
	a.logger.printf("saving shutdown snapshot at block %d -> %s (this can take several minutes)", head, a.cfg.shutdownSnapshotDir)
	start := time.Now()
	path, serr := snap.Save(a.state, a.cfg.shutdownSnapshotDir, head, "")
	if serr != nil {
		a.logger.printf("WARN shutdown snapshot at %d failed: %v", head, serr)
		return
	}
	a.logger.printf("shutdown snapshot %s at block %d in %s — restart with --load-snapshot=%s to resume here",
		path, head, snap.HumanDuration(time.Since(start)), path)
}

// recover replays blocks 1..a.recoverFrom from the local block_log
// against the freshly-genesised in-memory state. Required because the
// typed-stores (Account, Witness, Comment, CommentVote, WitnessVote)
// are NOT persisted in pebble — every restart starts them at genesis,
// so we must re-apply the canonical block_log to bring them up to the
// persisted DGPO head. Callers should invoke this BEFORE wiring the
// per-block onBlockApplied callback that re-appends to the writer.
//
// Uses skip-sigs (the canonical bytes already passed validation
// upstream) and the daemon's batched-commit setting to keep the
// catch-up fast. Aborts at the first apply failure — a corrupted
// block_log or pebble in an inconsistent state shouldn't silently
// produce a half-recovered head.
func (a *app) recover() error {
	if a.recoverFrom == 0 {
		return nil
	}
	r, err := blocklog.Open(a.blockLogPath)
	if err != nil {
		return fmt.Errorf("replay: open block_log: %w", err)
	}
	defer r.Close()
	target := a.recoverFrom
	if r.Head() < target {
		// pebble previously persisted further than the local block_log.
		// Replay only what the block_log can give us; the remaining
		// gap will close once the p2p layer pulls the missing blocks.
		a.logger.printf("replay: warning — state says head %d but the block_log ends at %d; replaying only %d blocks, peers supply the rest", a.recoverFrom, r.Head(), r.Head())
		target = r.Head()
	}
	if target == 0 {
		return nil
	}
	start := a.recoverStart
	if start < 1 {
		start = 1
	}
	if start > target {
		a.logger.printf("replay: state is already at block %s, the end of the block_log — nothing to replay", groupDigits(uint64(start-1)))
		return nil
	}
	if start > 1 {
		a.logger.printf("replay: replaying blocks %s to %s from the local block_log (the part after the loaded snapshot)", groupDigits(uint64(start)), groupDigits(uint64(target)))
	} else {
		a.logger.printf("replay: rebuilding state from genesis — replaying %s blocks from the local block_log", groupDigits(uint64(target)))
	}
	progress := newReplayProgress(start, target, time.Now())
	// No fsync at the batch boundaries while replaying: a replay that dies
	// starts over (from genesis or the snapshot), so the synced K/V buys
	// nothing, and each fsync waits for the ext4 journal behind the cold
	// tier's compaction writes - a quarter of the replay's wall time at
	// ~27M. The flush after the loop syncs once.
	if a.chain.CommitBatchSize() > 1 && a.cfg.commitBatchFsync {
		a.chain.SetCommitBatch(a.chain.CommitBatchSize(), false)
		defer a.chain.SetCommitBatch(a.chain.CommitBatchSize(), true)
	}
	for n := start; n <= target; n++ {
		raw, err := r.Block(n)
		if err != nil {
			return fmt.Errorf("replay: read block %d: %w", n, err)
		}
		if _, err := a.chain.ApplyBlock(raw, n, chain.ApplyOptions{
			SkipBlockSignature:        true,
			SkipTransactionSignatures: true,
			Sync:                      false,
			RawBlockRetained:          true, // Reader.Block allocates per call
		}, nil); err != nil {
			return fmt.Errorf("replay: apply block %d: %w", n, err)
		}
		// Spill frozen comments/votes to the cold tier DURING recovery too.
		// recover() runs before the afterApply callback is wired, so without
		// this a full-chain resume (replaying tens of millions of blocks)
		// would accumulate every comment resident and OOM in the dense era —
		// the exact failure the cold tier exists to prevent. Mirrors
		// afterApply's post-commit eviction.
		if a.commentStore != nil {
			a.commentStore.SetColdByRootEnabled(!hardfork.SetForBlock(n).Has(hardfork.HF17))
			a.commentStore.EvictFrozen(a.voteStore, a.evictBudget)
			if cwErr := a.commentStore.ColdWriteErr(); cwErr != nil {
				a.fatalf("cold tier lost a write — refusing to apply further blocks: %v", cwErr)
			}
			// FAIL-STOP on a lost cold write: the row was dropped from RAM
			// before the write failed, so continuing would apply blocks over a
			// hole in state and then snapshot it. Stop here instead.
			if cwErr := a.commentStore.ColdWriteErr(); cwErr != nil {
				a.fatalf("cold tier lost a write — refusing to apply further blocks: %v", cwErr)
			}
		}
		if n%replayProgressEvery == 0 {
			a.logger.printf("%s", progress.line(n, raw, time.Now()))
		}
	}
	// Flush any partial batch so the next start sees a consistent
	// head if it crashes here.
	if a.chain.CommitBatchSize() > 1 {
		if err := a.chain.FlushBatch(true); err != nil {
			return fmt.Errorf("replay: flush: %w", err)
		}
	}
	a.logger.printf("%s", progress.done(time.Now()))
	return nil
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
// startPprof serves net/http/pprof at --pprof-addr, if set. DefaultServeMux
// carries the handlers registered by the net/http/pprof import.
func (a *app) startPprof() {
	if a.cfg.pprofAddr == "" {
		return
	}
	go func() {
		a.logger.printf("pprof listening on %s (heap: /debug/pprof/heap)", a.cfg.pprofAddr)
		if err := http.ListenAndServe(a.cfg.pprofAddr, nil); err != nil {
			a.logger.printf("WARN pprof server: %v", err)
		}
	}()
}

func (a *app) run() error {
	seeds := []string{}
	if !a.cfg.disableMainSeeds {
		seeds = append(seeds, network.MainnetSeeds...)
	}
	if a.cfg.extraSeeds != "" {
		// Comma-separated so several can be pinned in one flag. Pinning
		// specific peers is the ONLY lever measured to matter for inbound
		// transaction latency: peer COUNT did nothing (12 -> 24 left p50
		// unchanged), but the TX-FIRST ranking shows the win distribution is
		// extremely concentrated — one peer was first to offer 62% of all
		// transactions. Which peers we hold decides how early we see traffic;
		// how many we hold does not.
		for _, sd := range strings.Split(a.cfg.extraSeeds, ",") {
			if sd = strings.TrimSpace(sd); sd != "" {
				seeds = append(seeds, sd)
			}
		}
	}
	a.logger.printf("starting (seeds=%d, listen=%q, target=%d)", len(seeds), a.cfg.listenEndpoint, a.cfg.targetBlock)

	// Block production, either mode. Dry run builds/signs/verifies and discards;
	// real production applies + broadcasts, but only when the chain's registered
	// signing key matches ours.
	//
	// This gate covers BOTH modes. It previously read `if produceDryRun`, so
	// --produce silently did nothing at all: the plugin was never constructed,
	// a.witness stayed nil, and the start below (correctly guarded on non-nil)
	// never fired. Symptom was a node that looked healthy with zero witness log
	// lines of any kind.
	if a.cfg.produceDryRun || a.cfg.produceReal {
		producers, perr := loadWitnessKeys(a.cfg.witnessKeyFile)
		if perr != nil {
			return perr
		}
		names := make([]string, 0, len(producers))
		for _, pr := range producers {
			names = append(names, pr.Account)
		}
		a.witness = witness.New(a.chain, a.state, producers)
		a.witness.DryRun = a.cfg.produceDryRun
		a.witness.DryRunEverySlot = a.cfg.produceEverySlot
		if a.cfg.produceDryRun {
			// Dry run needs no broadcaster and starts immediately: it never
			// publishes anything, so it does not depend on the p2p node.
			a.witness.Start()
			a.logger.printf("block production DRY RUN active for %v (every-slot=%v) — blocks are built, signed, verified and DISCARDED; never applied, never broadcast",
				names, a.cfg.produceEverySlot)
		} else {
			// REAL production is started later, once the p2p node exists, so it
			// has somewhere to broadcast. Starting it here would mean producing
			// blocks with no way to publish them.
			a.logger.printf("REAL block production armed for %v — will produce ONLY when the schedule names one of these AND the chain's registered signing key matches ours; otherwise the slot is logged and skipped",
				names)
		}
	}

	// Read-only JSON-RPC API (opt-in). Serves committed state; independent of
	// the p2p layer. Bind loopback unless deliberately exposed.
	if a.cfg.apiEndpoint != "" {
		// get_key_references answers from a public-key index. State replayed or
		// snapshotted before that index existed carries no rows, and a
		// half-populated index answers WRONG rather than slow, so backfill it
		// once before the API accepts traffic.
		//
		// ONE-TIME per pre-index snapshot: measured at 52s for mainnet's 2.6M
		// authorities, during which the apply lock is held. That is paid while
		// the node is still far behind the tip, and once built the index lives
		// in normal state, so it is carried by any snapshot taken afterwards.
		start := time.Now()
		if n, err := a.chain.EnsureKeyLookupIndex(); err != nil {
			a.logger.printf("WARN api: key-lookup index build failed: %v", err)
		} else if n > 0 {
			a.logger.printf("api: built key-lookup index over %d authorities in %s (one-time; carried by later snapshots)", n, time.Since(start).Round(time.Millisecond))
		}
		start = time.Now()
		if n, err := a.chain.EnsureSavingsWithdrawByToIndex(); err != nil {
			a.logger.printf("WARN api: savings by-recipient index build failed: %v", err)
		} else if n > 0 {
			a.logger.printf("api: built savings by-recipient index over %d rows in %s", n, time.Since(start).Round(time.Millisecond))
		}
		apiSrv := api.New(a.chain)
		// On a non-loopback (public) endpoint, disable the unbounded-scan
		// methods so a remote caller can't stall block application with an
		// O(store) scan under the apply read lock. Loopback keeps them enabled.
		if !apiEndpointIsLoopback(a.cfg.apiEndpoint) {
			n := apiSrv.RestrictForPublicEndpoint()
			a.logger.printf("api: non-loopback endpoint %q — disabled %d unbounded-scan methods (DoS guard)", a.cfg.apiEndpoint, n)
		}
		httpSrv := &http.Server{Addr: a.cfg.apiEndpoint, Handler: apiSrv.Registry().HTTPHandler()}
		go func() {
			a.logger.printf("api: serving JSON-RPC on http://%s (%d methods)", a.cfg.apiEndpoint, len(apiSrv.Registry().Names()))
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				a.logger.printf("api: server error: %v", err)
			}
		}()
		defer httpSrv.Close()
	}

	// Wire the per-block hook BEFORE starting the sync delegate so the
	// first APPLIED already lands in the block_log.
	stopCh := make(chan struct{}, 1)
	a.chain.SetOnBlockApplied(func(blockNum uint32, id block.ID) {
		raw := a.chain.RecentBlockBytes(blockNum)
		if raw == nil {
			a.logger.printf("WARN no raw bytes for applied block %d (cache miss)", blockNum)
		} else if err := a.afterApply(raw, blockNum, id); err != nil {
			a.logger.printf("ERROR post-apply hook block %d: %v", blockNum, err)
		}
		if a.cfg.targetBlock > 0 && blockNum >= a.cfg.targetBlock {
			select {
			case stopCh <- struct{}{}:
			default:
			}
		}
	})
	// Fork-aware path: a block's irreversible side-effects (block_log append +
	// eviction) run only when it COMMITS (FinalizeReversible), never on the
	// reversible tip a reorg might undo. onBlockApplied does NOT fire for
	// reversible blocks, so this is the sole side-effect path when
	// --fork-switching is on. raw is handed in directly (no cache lookup).
	a.chain.SetOnBlockFinalized(func(blockNum uint32, raw []byte) {
		// The id selects the cached compressed form; a miss just means the
		// block gets compressed locally.
		id, _ := a.chain.RecentBlockIDForNum(blockNum)
		if err := a.afterApply(raw, blockNum, id); err != nil {
			a.logger.printf("ERROR post-finalize hook block %d: %v", blockNum, err)
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
		UserAgent:          "gopherd/0.1.0",
		MaxPeers:           a.cfg.maxPeers,
		MaxOutboundPeers:   a.cfg.maxOutboundPeers,
		MaxConcurrentDials: a.cfg.maxConcurrentDials,
	}, a.chain, nil)
	syncCfg := p2psync.Config{
		LogAdaptiveBatch: a.cfg.logAdaptiveBatch,
		Serve:            a.cfg.p2pServe,
		LogWorkTiming:    a.cfg.logWorkTiming,
		ForkAware:        a.cfg.forkSwitching,
	}
	// Feed inbound transactions to the witness mempool. Supplying this also
	// turns ON transaction fetching in the sync layer; without it we record tx
	// inventory as known, never request the bodies, and every block we produce
	// is empty.
	//
	// Wired for the DRY RUN too, deliberately. Dry run is the safe way to
	// exercise this whole path against real mainnet traffic — fetch, admit,
	// select, size-budget — while never publishing a block. Gating it on real
	// production would mean the only way to test the mempool is to test it in
	// production, which is precisely the trade this dry-run mode exists to
	// avoid.
	if a.witness != nil && (a.cfg.produceReal || a.cfg.produceDryRun) {
		syncCfg.Mempool = a.witness
		// Relay transactions onward. Without it we are a pure SINK: we only ever
		// learn of a transaction when a peer volunteers it, which puts us at the
		// end of every propagation chain. Measured effect: we hold 2 pending
		// transactions at the instant we build while the real witness holds
		// 28-33 — we receive the right transactions, just too late to include
		// them.
		syncCfg.RelayTransactions = a.cfg.relayTransactions
		// Evict from the CHAIN's apply path, not the p2p layer. Blocks applied
		// via drainForward (out-of-order buffering) or re-applied during a
		// reorg never pass back through sync, so a p2p-side hook misses them —
		// measured live at only ~46% coverage, with the queue still growing
		// ~356/min toward the cap where it would stop admitting fresh
		// transactions and produce empty blocks again.
		w := a.witness
		a.chain.SetOnTxApplied(func(txID [20]byte) { w.RemoveIncludedTxID(txID) })
	}
	// RC-REJECTION DIAGNOSTIC: our RC accounting is measurably STRICTER than
	// hived's — the block comparison caught us refusing a transaction the real
	// block included. Log the payer and the error so the discrepancy can be
	// compared against hived's rc_api for that exact account and block, instead
	// of being re-derived from scratch later.
	if a.cfg.produceDryRun {
		chain.SetRCRejectLog(func(ops []op.Op, err error) {
			payer := ""
			if len(ops) > 0 {
				payer = ops[0].Tag.Name()
			}
			fmt.Fprintf(os.Stderr, "RC-REJECT: first-op=%s err=%v\n", payer, err)
		})
	}
	// ADMISSION AUDIT: run our mempool filter over the transactions of every
	// real block, before applying it. Every one of them is valid by definition,
	// so a refusal here is a proven over-refusal — the direct measurement of
	// whether the filter is costing us real traffic, independent of what any
	// peer happened to offer us.
	if a.cfg.produceDryRun {
		var auditTotal, auditRefused int
		auditReasons := map[string]int{}
		var auditMu gosync.Mutex
		a.chain.SetAuditAdmission(func(raw []byte, num uint32) {
			// ONLY audit at the tip. This re-runs full authority verification
			// (secp256k1 recovery) for every transaction in every block, inside
			// ApplyBlock under the write lock — it roughly doubles signature
			// work on the hot path. Ungated, it cost us tip-following: the node
			// fell ~290s behind, which silenced the dry run (its freshness gate
			// fired) and stopped the very measurement the audit was serving.
			//
			// A diagnostic that changes the behaviour it measures is worse than
			// no diagnostic. At the tip there is a 3s slot of slack per block,
			// which is ample; during catch-up there is none.
			if hdr, _, err := block.UnmarshalSignedHeader(raw); err == nil {
				if int64(time.Now().Unix())-int64(hdr.Timestamp) > 30 {
					return
				}
			}
			total, refusals := a.chain.AuditBlockAdmission(raw)
			if total == 0 {
				return
			}
			auditMu.Lock()
			auditTotal += total
			auditRefused += len(refusals)
			for _, r := range refusals {
				auditReasons[witness.ShortReason(r)]++
			}
			t, ref := auditTotal, auditRefused
			snap := make(map[string]int, len(auditReasons))
			for k, v := range auditReasons {
				snap[k] = v
			}
			auditMu.Unlock()
			if len(refusals) > 0 {
				fmt.Fprintf(os.Stderr,
					"ADMISSION AUDIT block %d: REFUSED %d/%d valid transactions (cumulative %d/%d = %.1f%%) %v\n",
					num, len(refusals), total, ref, t, 100*float64(ref)/float64(t), snap)
			}
		})
	}
	// DRY RUN comparison: when the real block for a height we assembled arrives,
	// diff the transaction sets. Block assembly is otherwise unverifiable
	// without publishing — and publishing a wrong block costs a slot and gets us
	// dropped by peers.
	if a.witness != nil && a.cfg.produceDryRun {
		w := a.witness
		syncCfg.OnBlockApplied = func(raw []byte, num uint32, _ *p2pnode.Peer) {
			w.CompareProducedBlock(num, chain.BlockTransactionIDs(raw))
		}
	}
	syncDel := p2psync.New(syncCfg, a.chain)
	a.p2p.SetDelegate(syncDel)
	if err := a.p2p.Start(); err != nil {
		return fmt.Errorf("p2p start: %w", err)
	}
	syncDel.Start()
	a.logger.printf("node ready in %.1fs — head %d, connecting to peers (catch-up begins once peers handshake)",
		time.Since(procStart).Seconds(), a.headBlockOrZero())
	defer syncDel.Stop()
	defer a.p2p.Stop()

	// REAL block production starts here, not earlier: it publishes through the
	// sync delegate, which must exist first.
	//
	// The witness hands each produced block to syncDel.PublishBlock, which
	// enqueues it on the SAME single-writer apply path inbound blocks use. That
	// path applies it reversibly through the fork-choice engine and advertises
	// it by block ID. The witness never applies or broadcasts a block itself —
	// doing either directly is what lost howo block 108887429 on 2026-08-10.
	//
	// The witness plugin still refuses to produce unless the chain's registered
	// signing key for the scheduled witness matches the key we hold, so arming
	// this does not by itself mean blocks will be published.
	if a.witness != nil && a.cfg.produceReal {
		a.witness.Publisher = syncDel
		a.witness.Start()
		a.logger.printf("REAL block production STARTED — blocks will be applied (reversibly, fork-aware) and ADVERTISED to peers when the key matches")
	}

	// Diagnostic: periodically dump the sync delegate's load-shedding
	// counters so we can see queue depth / outstanding budget / throttle
	// without attaching a debugger. Cheap (one line / 5s).
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		tick := 0
		for {
			select {
			case <-stopCh:
				return
			case <-t.C:
				s := syncDel.Stats()
				if w := syncDel.TxAdvertWinners(5); len(w) > 0 {
					a.logger.printf("TX-FIRST (peers closest to where transactions enter): %v", w)
				}
				if tick++; tick%12 == 0 {
					if r := syncDel.TxRejectReasons(5); len(r) > 0 {
						a.logger.printf("TX-REJECT (why fetched transactions were refused, since start): %v", r)
					}
				}
				awN, awMean, awMax, awOver := a.chain.AdmitLockWaitStats()
				a.logger.printf("STATS peers=%d qdepth=%d/%d inhibit=%v outstanding=%d distinct=%d inflight=%d throttled=%d qfull=%d reorderDrop=%d invIgnored=%d syncIDsRefused=%d invRx=%d tipStallKicks=%d tipSync=%d comp(rx=%d zstd=%d hit=%d err=%d) mempool(inv=%d fetch=%d acc=%d dup=%d rej=%d evict=%d ovf=%d depth=%d relay=%d served=%d unrequested=%d) admitWait(n=%d meanMs=%d maxMs=%d over100ms=%d) forkdb=%d rcStaleMax=%d",
					s.PeerCount, s.QueueDepth, s.QueueCapacity, s.FetchInhibited, s.OutstandingTotal, s.DistinctOutstanding, s.GlobalInflight,
					s.OutstandingThrottled, s.DroppedQueueFull, s.DroppedReorder, s.InvIgnoredCatchup, s.SyncIDsRefusedCatchup, s.InvReceived, s.TipStallKicks, s.TipSyncHandshakes,
					s.CompressedReceived, s.CompressedDecompressed, s.CompressedCacheHits, s.CompressedDecodeErrors,
					s.TxInvOffered, s.TxFetched, s.TxAccepted, s.TxDuplicate, s.TxRejected, s.TxEvicted, s.TxOverflowed, s.MempoolDepth, s.TxRelayed, s.TxServed, s.TxUnrequested,
					awN, awMean, awMax, awOver, a.chain.ForkDBLen(), objects.RCStaleMaxCount())
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		a.logger.printf("shutting down (signal)...")
	case <-stopCh:
		a.logger.printf("shutting down (target block %d reached)...", a.cfg.targetBlock)
	case <-a.fatalCh:
		a.logger.printf("shutting down (unrecoverable state error)...")
	}
	return nil
}

// afterApply is invoked from chain.SetOnBlockApplied (i.e. after the
// block has already passed the full validator pipeline). We append it to
// the block_log writer + diff against --verify-against if set.
func (a *app) afterApply(rawBlock []byte, blockNum uint32, id block.ID) error {
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
	if a.writer != nil {
		// A block that arrived compressed from a peer is already in block_log
		// form; store that payload instead of recompressing the raw bytes at
		// level 15 here, on the apply goroutine (it was half its CPU).
		var err error
		if payload, dictNum, hasDict, ok := a.chain.RecentCompressedBlock(blockNum, id); ok && a.cfg.compressBlocks {
			err = a.writer.AppendCompressed(payload, dictNum, hasDict)
			a.blocksStoredAsReceived.Add(1)
		} else {
			err = a.writer.Append(rawBlock)
			a.blocksRecompressed.Add(1)
		}
		if err != nil {
			return fmt.Errorf("append block %d: %w", blockNum, err)
		}
	}
	// Post-commit: spill frozen comments + votes to the cold tier when over
	// the resident cap (no-op when the cold tier is off). This is the P2P
	// apply path — the sync delegate calls chain.ApplyBlock directly, which
	// fires this SetOnBlockApplied callback but NOT app.applyBlock, so without
	// this the cold tier never engages during a P2P sync: resident comments
	// accumulate unbounded and the heap climbs into the mem-limit, GC-crawling
	// the dense HF17+ era (observed: genesis P2P sync cold-empty, RSS pinned at
	// the limit, throughput collapsing 5.5K->180 bps by ~19M). Mirrors
	// applyBlock's post-commit eviction.
	if a.commentStore != nil {
		a.commentStore.SetColdByRootEnabled(!hardfork.SetForBlock(blockNum).Has(hardfork.HF17))
		a.commentStore.EvictFrozen(a.voteStore, a.evictBudget)
		if cwErr := a.commentStore.ColdWriteErr(); cwErr != nil {
			a.fatalf("cold tier lost a write — refusing to apply further blocks: %v", cwErr)
		}
	}
	// Ephemeral resume marker: periodically record the applied head so a restart
	// with --recover-block-log can rebuild to here from the mirror instead of
	// re-syncing from 0. Only meaningful for --no-block-log (the writer's head
	// is the anchor otherwise). Best-effort; a failed write just costs a little
	// re-replay next time.
	if a.cfg.noBlockLog && blockNum%resumeMarkerInterval == 0 {
		if err := writeResumeMarker(a.cfg.dataDir, blockNum); err != nil {
			a.logger.printf("WARN write resume marker at %d: %v", blockNum, err)
		}
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

	// Post-commit: spill frozen comments + votes to the cold tier when
	// over the resident cap (no-op when the cold tier is off). Keeps a
	// full-chain node's RAM bounded. Post-HF17 skips the dead by-root
	// cold writes.
	if a.commentStore != nil {
		a.commentStore.SetColdByRootEnabled(!hardfork.SetForBlock(blockNum).Has(hardfork.HF17))
		a.commentStore.EvictFrozen(a.voteStore, a.evictBudget)
		if cwErr := a.commentStore.ColdWriteErr(); cwErr != nil {
			a.fatalf("cold tier lost a write — refusing to apply further blocks: %v", cwErr)
		}
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

// loadWitnessKeys reads a file of `account WIF` lines into witness Producers.
//
// Keys live in a file rather than a flag on purpose: command lines are visible
// in ps to every user on the box, so a --witness-key flag would leak signing
// keys to anyone with shell access.
//
// Blank lines and # comments are skipped. Each key is checked against nothing
// on-chain here — a key that does not match the witness's registered signing
// key still exercises the pipeline, and the dry run's self-verification will
// report the mismatch rather than silently passing.
func loadWitnessKeys(path string) ([]witness.Producer, error) {
	// This file holds the private keys that sign blocks. Keys are deliberately
	// not accepted as flags because command lines are visible in ps — but that
	// only moves the exposure to the file, so check that the file itself is not
	// readable by anyone else. A leaked signing key lets an attacker produce
	// blocks as this witness.
	if fi, serr := os.Stat(path); serr == nil {
		if mode := fi.Mode().Perm(); mode&0o077 != 0 {
			fmt.Fprintf(os.Stderr, "gopherd: WARNING - witness key file %s is mode %04o, readable by group/other. These are BLOCK-SIGNING PRIVATE KEYS; run: chmod 600 %s\n", path, mode, path)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("witness-keys: %w", err)
	}
	var out []witness.Producer
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("witness-keys line %d: want `account WIF`, got %d fields", i+1, len(fields))
		}
		priv, derr := crypto.DecodeWIF(fields[1])
		if derr != nil {
			return nil, fmt.Errorf("witness-keys line %d (account %q): %w", i+1, fields[0], derr)
		}
		out = append(out, witness.Producer{
			Account:     fields[0],
			PrivateKey:  priv,
			PublicKey33: priv.PublicKey(),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("witness-keys %s: no entries", path)
	}
	return out, nil
}

// registerTypedSubsystems registers every typed store a full node needs.
//
// The set must match the producing process exactly: the snapshot loader fails
// if the snapshot carries a subsystem the DB has not registered, and the
// persiststore restore has the same requirement. Factored out so the
// snapshot, persist-state and from-genesis paths cannot drift apart — a
// mismatch between them is silent until a load fails.
func registerTypedSubsystems(boot *state.Session) {
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

// attachColdTier wires the comment/vote cold tier when --comment-resident-cap
// is set. Returns (nil, nil, nil, nil) when the cap is 0 (cold tier off).
//
// Must run BEFORE any bulk load so the multi-GB comment corpus spills to disk
// during the load instead of piling up resident — a full-chain load with the
// tier unattached OOMs.
// coldHeadStampName is written into the cold-tier dir on clean shutdown,
// recording the head block the persist-state container was saved at.
const coldHeadStampName = "head.stamp"

// writeColdHeadStamp records `head` in the cold-tier dir. Call only AFTER the
// cold tier has been closed/flushed, so the stamp cannot claim more than the
// data on disk.
func writeColdHeadStamp(coldDir string, head uint32) error {
	if coldDir == "" {
		return nil
	}
	tmp := filepath.Join(coldDir, coldHeadStampName+".tmp")
	if err := os.WriteFile(tmp, []byte(strconv.FormatUint(uint64(head), 10)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(coldDir, coldHeadStampName))
}

// readColdHeadStamp returns the head the cold tier was last cleanly closed at.
// Reports false when absent (never cleanly closed, or written by an older
// build), which callers must treat as "cannot verify" — not as "fine".
func readColdHeadStamp(coldDir string) (uint32, bool) {
	if coldDir == "" {
		return 0, false
	}
	b, err := os.ReadFile(filepath.Join(coldDir, coldHeadStampName))
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

func attachColdTier(stateDB *state.Database, boot *state.Session, cfg flags) (objects.ColdStore, *objects.CommentStore, *objects.CommentVoteStore, error) {
	if cfg.residentCap <= 0 {
		return nil, nil, nil, nil
	}
	backendName := cfg.coldBackend
	if backendName == "persist" && cfg.coldDir == "" {
		return nil, nil, nil, fmt.Errorf("--cold-backend=persist requires a stable --cold-dir")
	}
	c, err := objects.OpenColdTierBackend(cfg.coldDir, backendName)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open cold tier: %w", err)
	}
	cs := objects.EnsureCommentStore(boot)
	cvs := objects.EnsureCommentVoteStore(boot)
	cs.SetColdTier(c)
	cs.SetResidentCap(cfg.residentCap)
	cvs.SetColdTier(c)
	fmt.Fprintf(os.Stderr, "gopherd node: cold tier ON — resident cap %d, evict budget %d/block, dir %s\n",
		cfg.residentCap, cfg.evictBudget, c.Dir())
	return c, cs, cvs, nil
}

// snapshotManifestBlock reads just the block number out of a snapshot manifest,
// so the cold sidecar can be validated against it before the snapshot is loaded.
func snapshotManifestBlock(dir string) (uint32, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return 0, err
	}
	var m struct {
		BlockNum uint32 `json:"block_num"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, err
	}
	if m.BlockNum == 0 {
		return 0, fmt.Errorf("manifest has no block_num")
	}
	return m.BlockNum, nil
}

// humanDirSize reports a snapshot directory's total size for the pre-load line,
// so the operator knows up front whether to expect seconds or minutes. Best
// effort: an unreadable dir just yields "size unknown" rather than failing a
// boot over a log line.
func humanDirSize(dir string) string {
	var total int64
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi != nil && !fi.IsDir() {
			total += fi.Size()
		}
		return nil
	})
	if err != nil || total == 0 {
		return "size unknown"
	}
	return fmt.Sprintf("%.1f GB", float64(total)/(1<<30))
}

// headBlockOrZero is a log-only convenience: never fail a status line over a
// head lookup.
func (a *app) headBlockOrZero() uint32 {
	if a.chain == nil {
		return 0
	}
	h, err := a.chain.HeadBlock()
	if err != nil {
		return 0
	}
	return h
}
