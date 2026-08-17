# WORK IN PROGRESS SOFTWARE, DO NOT USE IN PRODUCTION

# gopherd

A Go implementation of [hived](https://gitlab.syncad.com/hive/hive), the Hive
blockchain daemon. Pure Go, no cgo.

It replays the chain from genesis, syncs over the graphene P2P protocol, follows
the live mainnet head, serves a read-only JSON-RPC subset, and can run the block
production pipeline.

> **Naming:** the project, binary and Go module are all **gopherd**, so import
> paths read `gopherd/internal/...`. The checkout directory may still be named
> `hived-go` on older machines — Go does not require it to match the module.
> Environment switches are `GOPHERD_*`; the old `GHIVE_*` names still work but
> warn once.

## Status

Consensus is validated against mainnet by the strongest check available: applied
block IDs are **bit-identical** to the canonical chain, verified continuously
from genesis through the live head (block ~108.8M as of 2026-08).

| Area | State |
|---|---|
| Replay genesis → head | Working. Full chain, 0 apply errors. |
| P2P sync to live head | Working. Warm-start from a snapshot, catch up, follow the tip at gap 0. |
| Serving blocks to peers | Working (`--p2p-serve`). See the safety note below. |
| JSON-RPC (read side) | 64 methods. Consensus-node scope only — see `docs/api-implementation-map.md`. |
| Block production | Pipeline works end to end. Dry run validated live; real production is key-gated and unproven. |
| Fork switching | Implemented, default off (`--fork-switching`). |
| Transaction relay / mempool | **Not implemented.** Produced blocks carry no transactions. |

### Safety note on serving

This node caused two real mainnet disruptions (2026-07 and 2026-08) by
advertising block inventory it should not have. Both root causes are fixed and
covered by regression tests, and `docs/p2p-vs-hived.md` compares our gossip path
against hived's line by line — including the divergences that remain open.

`--p2p-serve` defaults **off**. Leave it off unless you have read that document.

## Requirements

- Go 1.26 or newer.

## Build

```
make            # builds bin/gopherd
make test       # all tests
make vet        # static analysis
```

## Running a node

Warm-start from a state snapshot and follow the live head:

```
gopherd node \
  --data-dir=/path/state \
  --backend=pebble \
  --load-snapshot=/path/snapshots/block-N \
  --comment-resident-cap=500000 \
  --cold-backend=pebble --cold-dir=/path/cold \
  --persist-state-dir=/path/pstate \
  --mem-limit-gib=22
```

`scripts/p2p_live_head.sh` wraps this with memory limits and the usual flags.

A few flags matter more than the rest:

- `--persist-state-dir` — mmap-backed typed state, the equivalent of hived's
  chainbase `shared_memory.bin`. Written on **clean shutdown** (SIGTERM); a
  restart then resumes at head in seconds instead of rebuilding multi-GB typed
  stores from a snapshot (~25 min). A `kill -9` skips it, so stop the node
  properly.
- `--comment-resident-cap` + `--cold-dir` — required for a full-chain node. The
  comment corpus does not fit in RAM; frozen comments spill to the cold tier.
  Watch `du -sh <cold-dir>`: growing while RSS stays flat is healthy, an empty
  cold dir during a full sync means eviction is not engaging and an OOM is
  coming.
- `--no-block-log` — needed on a snapshot warm start. Otherwise the node writes a
  *partial* block_log starting at snapshot-head+1, and the restart path replays
  that partial log from its own block 1 against empty typed stores.

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

**Real production refuses to produce when the chain's registered signing key for
the witness is not the key we hold**, logging `it's our turn to produce but NO
KEY MATCHES` and skipping the slot — what hived does. Emitting a block signed
with the wrong key is strictly worse than missing the slot: it is rejected, the
slot is missed anyway, and the slot is spent.

Measured live (impersonating a top-20 witness, dry run): assemble + merkle + sign
+ verify in **0.3-2.1ms** against the 3000ms slot budget. Those blocks carried
zero transactions, so treat it as a floor.

Known gap: the production loop polls every 500ms, where hived arms a timer for
the exact slot boundary. Fine for measuring build cost, not good enough for real
production.

## Layout

```
cmd/gopherd            — single binary: daemon + operator/dev tools
internal/protocol      — consensus types (no DB)
internal/fcraw         — FC binary codec (Hive's wire format)
internal/crypto        — sha256, ripemd160, secp256k1, base58 STM keys
internal/blocklog      — hived-compatible block_log reader/writer (+ zstd)
internal/state         — Pebble/mmap state, snapshots, cold tier, persiststore
internal/chain         — apply_block, evaluators, fork DB
internal/p2p           — graphene-compatible P2P: transport, node, sync, gossip
internal/api           — JSON-RPC server
internal/plugins       — witness block production
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

The `state-jsonl` → `snapshot-diff` pair is the workhorse for consensus
debugging: dump both implementations at the same block and diff.

## License

TBD — to match hived's upstream license once published.
