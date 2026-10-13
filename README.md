# WORK IN PROGRESS SOFTWARE, DO NOT USE IN PRODUCTION

# gopherd

A Go implementation of [hived](https://gitlab.syncad.com/hive/hive), the Hive
blockchain daemon.

It replays the chain from genesis, syncs over the graphene P2P protocol, follows
the live mainnet head, relays transactions, serves a read-only JSON-RPC subset,
and produces blocks.

> **Naming:** the project, binary and Go module are all **gopherd**, so import
> paths read `gopherd/internal/...`. The checkout directory may still be named
> `hived-go` on older machines — Go does not require it to match the module.
> Environment switches are `GOPHERD_*`; the old `GHIVE_*` names still work but
> warn once.

## Status

Consensus is validated against mainnet by the strongest check available: applied
block IDs are **bit-identical** to the canonical chain, verified continuously
from genesis through the live head (block ~109.1M as of 2026-08-18).

gopherd has produced real blocks on Hive mainnet, including blocks carrying
transactions, making Hive a genuinely multi-client network.

| Area | State |
|---|---|
| Replay genesis → head | Working. Full chain, 0 apply errors. |
| P2P sync to live head | Working. Warm-start from a snapshot, catch up, follow the tip at gap 0. |
| Serving blocks to peers | Working (`--p2p-serve`). See the safety note below. |
| Transaction relay / mempool | Working (`--relay-transactions`). |
| JSON-RPC (read side) | 71 methods. Consensus-node scope only — see `docs/api-implementation-map.md`. |
| Block production | Working. Dry run and real production both validated on mainnet; real production is key-gated. |
| Fork switching | Implemented, default off (`--fork-switching`). |
| Testnet | Joins **hived's** testnet: syncs, matches block IDs, and produces blocks hived accepts. See `docs/testnet.md`. |
| Zero-snapshot fast restart | **Not delivered.** `--persist-state-dir` exists but is slower than a snapshot load at mainnet scale. |

### Safety note on serving

This node caused two real mainnet disruptions (2026-07 and 2026-08) by
advertising block inventory it should not have. Both root causes are fixed and
covered by regression tests, and `docs/p2p-vs-hived.md` compares our gossip path
against hived's line by line — including the divergences that remain open.

`--p2p-serve` defaults **off**. Leave it off unless you have read that document.

## Requirements

### Software

- **Go 1.26 or newer** (see `go.mod`)
- **libzstd development headers + pkg-config**. gopherd is *not* pure Go: the
  block decoder binds libzstd through cgo, so `CGO_ENABLED=0` does not build.
  - Debian/Ubuntu: `sudo apt install build-essential pkg-config libzstd-dev`
    (verified against libzstd 1.4.8)
  - Fedora/RHEL: `sudo dnf install gcc pkgconf-pkg-config libzstd-devel`
  - macOS: `brew install zstd pkg-config`
- Linux is the tested platform. Nothing is known to be Linux-specific beyond the
  operational tooling, but other platforms are untested.

### Hardware

Figures below are **measured** on mainnet on 2026-10-13 (head ~110.7M), on a
24-thread box with 46 GB RAM and an NVMe SSD, running a tip-following node with
`--comment-resident-cap=2000000 --mem-limit-gib=12 --cache-mb=512
--cold-backend=pebble --cold-cache-mb=256`, under a 20 GB cgroup cap:

| Resource | Measured | Notes |
|---|---|---|
| RSS at tip | **8.1 GB** | Go heap 6.4 GB in use; 11.7 GB high-water, reached while loading a snapshot |
| Page cache | ~9 GB | pebble and cold-tier reads; reclaimable, but startup time depends on it |
| CPU at tip | 3–9% of one core | following the head, no RPC load |
| State (pebble) | 2.2 GB | |
| Cold tier | 100 GB | grows with the comment corpus; transiently ~150 GB while a snapshot loads |
| Snapshot | 35 GB | 33 GB of it is the comment store; keep the latest one |
| Snapshot write | 6–9.5 min | on shutdown (SIGTERM); give the service a stop timeout of an hour |
| Snapshot load | **1h45m** | the comment store re-spills into the cold tier at ~5 MB/s |
| Catch-up after load | ~20 s for 2.8 h of blocks | 120–330 blocks/s from peers |

Recommended for a tip-following node:

- **4+ cores**, **24 GB RAM** (16 GB for the node plus page cache),
  **NVMe or SSD**, **200 GB free disk** without a block_log.
- A spinning disk is not recommended and is untested. The cold tier is a
  random-access LSM store.

Additional, only if you keep a block_log (needed to replay from genesis):

- A block_log written by a p2p sync is **539 GB** (plus an 845 MB index) at
  110.7M blocks. It stores each block as the peer sent it — zstd level 3, not
  the level-15 recompression hived applies — so it is larger than hived's.
  `--no-block-log` avoids writing one on a snapshot warm start.

## Installation

```sh
git clone git@gitlab.syncad.com:hive/gopherd.git
cd gopherd
make            # builds bin/gopherd
make test       # all tests
make vet        # static analysis
```

`make` writes `bin/gopherd`. There is a single binary; every tool is a
subcommand (`gopherd help` lists them).

> Build straight to `bin/` (`go build -o bin/gopherd ./cmd/gopherd`). Plain
> `go build ./...` compiles but does **not** refresh `bin/gopherd`, which makes
> it easy to run a stale binary and misread the result.

Verify:

```sh
./bin/gopherd help
./bin/gopherd validate-blocklog /path/to/block_log   # re-derives merkle roots + block IDs
```

## Running a node

### Quick start (follow the live head from a snapshot)

```sh
gopherd node \
  --data-dir=/path/state \
  --backend=pebble \
  --no-block-log \
  --load-snapshot=/path/snapshots/block-N \
  --comment-resident-cap=500000 \
  --cold-backend=pebble --cold-dir=/path/cold \
  --mem-limit-gib=12 --cache-mb=512 \
  --api-endpoint=127.0.0.1:8090
```

`scripts/p2p_live_head.sh` wraps this with memory limits and the usual flags.

### Getting a snapshot

There is no published snapshot download. Produce one from a node that has
already synced, by stopping it cleanly:

```sh
gopherd node ... --shutdown-snapshot-dir=/path/snapshots
# SIGTERM (not kill -9) -> writes /path/snapshots/block-<head>/
```

Restarting needs **both** that snapshot and the same `--cold-dir`. Snapshots are
not self-contained unless the cold tier goes with them.

Without a snapshot, a node builds state by replaying a `block_log` from genesis,
which takes about 20 hours and a 539 GB log (see Hardware).

### Flags that matter more than the rest

- `--comment-resident-cap` + `--cold-dir` — **required for a full-chain node.**
  The comment corpus does not fit in RAM; frozen comments spill to the cold
  tier. Watch `du -sh <cold-dir>`: growing while RSS stays flat is healthy; an
  empty cold dir during a full sync means eviction is not engaging and an OOM is
  coming.
- `--no-block-log` — needed on a snapshot warm start. Otherwise the node writes a
  *partial* block_log starting at snapshot-head+1, and the restart path replays
  that partial log from its own block 1 against empty typed stores.
- `--mem-limit-gib` — counter-intuitive: **raising it slows startup.** The Go
  heap competes with the page cache over the snapshot. Same ~26 GB snapshot,
  measured: 22 GiB → 155 s, 12 GiB → 108 s, 12 GiB warm cache → 65 s.
- `--extra-seed` — repeatable, and accepts a comma-separated list. Worth pinning
  specific peers: 5 of 24 peers supplied 83% of our earliest transaction
  adverts, and which peers you hold decides how early you see traffic. Peer
  *count* does not (12 → 24 changed nothing).
- `--relay-transactions` — off by default. Without it the node only consumes
  transactions and sits at the end of every propagation chain.
- `--p2p-serve` — off by default. Read the safety note first.
- `--pprof-addr=127.0.0.1:6060` — live heap/CPU profiling. Essential for
  diagnosing memory behaviour; `go tool pprof -base` over a **multi-hour** window
  is the only reliable read (see Operating notes).
- `--persist-state-dir` — **does not deliver a fast restart at mainnet scale.**
  On the pebble cold tier the load re-spills the entire ~25 GB comment corpus and
  was still running at 22 minutes, slower than the snapshot load it replaces.
  Leave it off.

## Testnet

A `-tags testnet` build joins a **hived** testnet rather than running a chain of
its own.

```sh
scripts/testnet-up          # 2 hived + 1 gopherd, 5 witnesses, all producing
scripts/testnet-status      # block-ID agreement + who produced what
scripts/testnet-state-diff  # chain STATE comparison (see the warning below)
scripts/testnet-down        # stop everything, keep the data dirs
```

Testnet-only subcommands (they refuse to run on a mainnet build):

| Command | Purpose |
|---|---|
| `testnet-bootstrap --witnesses=a,b` | vest and vote as initminer so the init witnesses get elected — a fresh testnet gives them no votes, so without this only `initminer` ever produces |
| `testnet-bots` | load generator: a pool of accounts submitting random operations across 40 of the 51 operation types |

`testnet-bots` flags worth knowing:

```sh
--bots=6 --duration=3m --tps=8   # size, length and aggregate rate
--seed=NAME                      # deterministic accounts, keys and op choices
--setup-only                     # create and fund the accounts, then stop
--only-ops=transfer,vote         # restrict the workload
--endpoint=http://127.0.0.1:8751 # submit through this node
```

`--only-ops` matters more than it looks: a mixed workload cannot be compared
per-operation against hived, because the cost and the state effects depend on
which operations ran. Isolating one operation class is how several RC defects
were separated from each other. `--seed` makes a run that trips a bug replay
exactly.

Read `docs/testnet.md` before changing consensus code — in particular, block-ID
agreement does **not** prove state agreement, and every consensus bug found
during testnet bring-up was invisible to a block-ID comparison. Run
`scripts/testnet-state-diff`, and run `testnet-bots` before believing a change
is safe: submitting real traffic found bugs that replaying existing blocks
never could, because replay never exercises the submission path.

## Block production

Two modes, both requiring `--witness-keys FILE` (a file of `account WIF` lines —
keys are never taken as flags, since command lines are visible in `ps`).

```
--produce-dry-run   build, sign, self-verify, measure, DISCARD
--produce           build, sign, APPLY and BROADCAST — only if the key matches
```

**Dry run never applies and never broadcasts.** That is what makes it safe
against mainnet: a locally produced block that got applied would move our head to
a block no peer has, forking this node off the chain it is following.
`--produce-dry-run-every-slot` builds every slot (you are otherwise almost never
scheduled), which is what makes the timing data measurable.

**Real production refuses to produce when the chain's registered signing key for
the witness is not the key we hold**, logging `it's our turn to produce but NO
KEY MATCHES` and skipping the slot — what hived does. Emitting a block signed
with the wrong key is strictly worse than missing the slot: it is rejected, the
slot is missed anyway, and the slot is spent.

Measured live on mainnet: assemble + merkle + sign + verify in **0.3–7 ms**
against the 3000 ms slot budget.

Known gaps:

- The production loop polls rather than arming a timer for the exact slot
  boundary. Adequate for the current margin, not ideal.
- Produced blocks are a *subset* of what other witnesses produce: measured
  p50 inbound transaction lateness of ~0.6-1.4 s means some transactions reach us
  after we have already built. This is propagation position, not filtering —
  audits show the selector refuses nothing and drops nothing it holds.

## JSON-RPC API

`--api-endpoint=host:port` serves 71 read-only methods across `database_api`,
`condenser_api`, `rc_api`, `block_api`, `account_by_key_api`, and
`network_broadcast_api`.

Scope is **consensus-node only**. Account history, social features, market data
and reputation belong to HAFAH / Hivemind / HAF-market and are deliberately out
of scope — see `docs/api-implementation-map.md`.

```sh
curl -s -X POST http://127.0.0.1:8090 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"database_api.get_dynamic_global_properties","params":{},"id":1}'
```

`scripts/api_smoke.sh` exercises the surface.

## Operating notes

- **Stop with SIGTERM, never `kill -9`.** Snapshot and cold-tier flushes happen
  on clean shutdown.
- **Watch `forkdb=` in the STATS line.** It should sit at ~66. Unbounded growth
  there was a real leak (every block ever received was retained with its
  payload, ~200 MB/day) — it is now bounded, and the counter exists so a
  regression is visible.
- **Reading heap trends:** the live-heap series oscillates ±5 MB between GC
  cycles. Sub-hour windows produce spurious slopes *in both directions*. Fit a
  regression over ≥2 h of **steady-state-only** samples (exclude catch-up) and
  compare the drift to the residual noise before concluding anything.
- **Disk:** check free space before long runs; `bin/disk_guard.sh` and the
  disk-monitor timer exist for this.

## Layout

```
cmd/gopherd            — single binary: daemon + operator/dev tools
internal/protocol      — consensus types (no DB)
internal/fcraw         — FC binary codec (Hive's wire format)
internal/crypto        — sha256, ripemd160, secp256k1, base58 STM keys
internal/blocklog      — hived-compatible block_log reader/writer (+ zstd, cgo)
internal/state         — Pebble/mmap state, snapshots, cold tier, persiststore
internal/chain         — apply_block, evaluators, fork DB
internal/p2p           — graphene-compatible P2P: transport, node, sync, gossip
internal/api           — JSON-RPC server
internal/plugins       — witness block production + mempool
docs/                  — API map, fork-switching design, gopherd-vs-hived P2P
scripts/               — live-head runner, checkpoint + parity tooling
```

## Tools

`gopherd help` lists everything. The ones used most:

| Command | Purpose |
|---|---|
| `node` | run the daemon |
| `replay <block_log>` | apply every block through the evaluators |
| `benchmark <block_log>` | cold-start replay benchmark: wall-clock, peak RSS, ops/sec |
| `state-jsonl --data-dir` | deterministic JSONL state dump for line-diffing vs hived |
| `snapshot-diff a.jsonl b.jsonl` | report missing/extra/changed objects between dumps |
| `validate-blocklog <block_log>` | re-derive every merkle root and block ID |
| `compare-blocklog <ref> <other>` | byte-compare two block_logs |
| `testnet-bootstrap` | elect the genesis witnesses (testnet build only) |
| `testnet-bots` | random-operation load generator (testnet build only) |

The `state-jsonl` → `snapshot-diff` pair is the workhorse for consensus
debugging: dump both implementations at the same block and diff.

## License

TBD — to match hived's upstream license once published.
