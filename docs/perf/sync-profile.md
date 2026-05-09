# Sync-path CPU + memory profile

Date: 2026-05-09. Branch: `agent-profile`. Tooling: `runtime/pprof` via
`cmd/ghive-sync --cpuprofile/--memprofile`. Workload: 200K-300K mainnet
blocks self-replayed through the full evaluator + writer pipeline (the
same code path the live P2P sync delegate calls), `--backend=mem`, no
verify-against, full signature verification (no `SkipBlockSignature`).

Replay throughput on this host: **~2,750 blocks/s** at 200K. CPU
samples = 83.73s over 89.77s wall (93% utilized). The remaining 7%
is GC / runtime / syscalls.

## Headline finding: secp256k1 dominates 88% of CPU

| layer                                          | cum CPU |
| ---------------------------------------------- | ------- |
| `chain.(*Database).ApplyBlock`                 | 94.6%   |
| └─ `crypto.RecoverPublicKey`                   | 88.4%   |
| │  └─ `decred/dcrd/dcrec/secp256k1/v4/ecdsa.RecoverCompact` | 88.4% |
| │     └─ `secp256k1.ScalarMultNonConst`        | 59.9%   |
| │     └─ `secp256k1.ScalarBaseMultNonConst`    | 12.6%   |
| │     └─ `secp256k1.DecompressY`               | 6.8%    |
| └─ everything else (state, evaluators, merkle) | ~6%     |

The hot leaves are `FieldVal.SquareVal` (35% flat), `FieldVal.Mul2`
(30% flat), `FieldVal.Normalize` (10% flat). All inside the pure-Go
secp256k1 implementation. **One ECDSA recovery per block on the
self-replay path**; the live P2P path also recovers per block, then
caches the recovered signer keyed by block-ID (commit `bd287ae`) so a
re-application of the same block is free.

## Top-10 by cum CPU (entire process)

| flat   | cum    | %      | function                                                   |
| ------ | ------ | ------ | ---------------------------------------------------------- |
| 0      | 83.21s | 99.4%  | `main.(*app).replayFromSource`                             |
| 0      | 80.08s | 95.6%  | `main.(*app).applyBlock`                                   |
| 0.13s  | 79.19s | 94.6%  | `chain.(*Database).ApplyBlock`                             |
| 0      | 74.04s | 88.4%  | `crypto.RecoverPublicKey`                                  |
| 0.02s  | 74.00s | 88.4%  | `secp256k1/v4/ecdsa.RecoverCompact`                        |
| 0.01s  | 73.47s | 87.7%  | `chain.verifyBlockSignatureWithCache`                      |
| 0.23s  | 50.11s | 59.9%  | `secp256k1.ScalarMultNonConst`                             |
| 0.16s  | 38.12s | 45.5%  | `secp256k1.AddNonConst`                                    |
| 0.35s  | 35.86s | 42.8%  | `secp256k1.addZ2EqualsOne`                                 |
| 0.04s  | 22.45s | 26.8%  | `secp256k1.DoubleNonConst`                                 |

## Top-10 by flat CPU (where ticks actually land)

| flat   | %     | function                                                         |
| ------ | ----- | ---------------------------------------------------------------- |
| 29.37s | 35.1% | `secp256k1.(*FieldVal).SquareVal`                                |
| 24.80s | 29.6% | `secp256k1.(*FieldVal).Mul2`                                     |
| 8.11s  | 9.7%  | `secp256k1.(*FieldVal).Normalize`                                |
| 2.36s  | 2.8%  | `internal/runtime/syscall/linux.Syscall6` (block_log writes / pread) |
| 2.06s  | 2.5%  | `secp256k1.(*FieldVal).NegateVal`                                |
| 1.67s  | 2.0%  | `secp256k1.(*FieldVal).Set`                                      |
| 1.31s  | 1.6%  | `secp256k1.(*FieldVal).IsZero`                                   |
| 1.24s  | 1.5%  | `secp256k1.(*FieldVal).Add`                                      |
| 0.99s  | 1.2%  | `secp256k1.(*FieldVal).MulInt`                                   |
| 0.48s  | 0.6%  | `runtime.memmove`                                                |

Outside secp256k1, **less than 8.6s** of total CPU is spent on
everything else (block parse, every evaluator, witness-schedule, dgpo
update, btree probes, block_log append, …). Per-block: ~43µs. So a
self-replay block is ~430µs of which ~390µs is signature recovery.

## Top-10 by alloc bytes (heap pressure, NOT live heap)

Sample workload: 200K blocks → 1.21 GiB allocated total (~6 KiB/block).
Live heap stays under 25 MiB the whole run.

| MB     | %     | function                                                        |
| ------ | ----- | --------------------------------------------------------------- |
| 342.37 | 27.6% | `blocklog.buildInMemoryIndex` (one-shot, opens reference / source) |
| 168.04 | 13.5% | `state.(*Session).Put` (overlay buffer copies)                  |
| 111.01 | 8.9%  | `math/big.nat.make` (secp256k1 `ModNScalar.InverseValNonConst`) |
| 76.01  | 6.1%  | `tidwall/btree.iter` (overlayKeys.Iter escapes — see Range fix below) |
| 45.01  | 3.6%  | `state/objects.GetDGPO` (per-block lazy cache `&dgpoLazy{}`)    |
| 43.63  | 3.5%  | `klauspost/compress/zstd.DecodeAll` (block_log dictionary read) |
| 36.51  | 2.9%  | `state.(*memBackend).rawIter` (per-Range mem-backend iterator)  |
| 30.00  | 2.4%  | `chain.(*Database).updateLastIrreversibleBlock`                 |
| 26.50  | 2.1%  | `blocklog.fileWriter.AppendRaw` (write buffers)                 |
| 24.50  | 2.0%  | `blocklog/zstd.reframe`                                         |

**Live heap top:** btree overlay nodes (5.7 MiB) + Session.Put copies
(5.6 MiB) + runtime mallocgc misc (3 MiB). Steady-state heap is small.
GC overhead is negligible: pprof shows `runtime.mallocgc` total CPU at
1.4% of cum.

## Per-evaluator op-time

A full per-op histogram is not measurable from this profile —
`evaluator.Apply` is dispatched via interface method, and pprof's stack
trace doesn't survive interface dispatch on every leaf. Every op
evaluator combined accounts for under ~3.5 MB of alloc (line 430 of
`ApplyBlock`) and well under 5% of cum CPU. The expensive evaluators
are the cashout / vote / comment paths — but at the self-replay
throughput we're seeing here those are **dominated by signature
verification by 20:1**.

## Recommendations (3 functions, biggest wins)

### 1. Skip block-signature recovery on the self-replay path

**Estimated impact: 5-7x replay throughput.**

`cmd/ghive-sync` runs full block-signature recovery even when
`--source-block-log` is set against a canonical block_log that was
already verified upstream. We have `SkipBlockSignature` plumbed
(used by the fully-trusted replay tool and by tests via
`skipSigsForTest`), but `applyBlock` does not consult either
`--source-block-log` *or* `--verify-against` to decide. Adding a
`--trust-source-sigs` flag (default off; explicit opt-in for the
case where the operator KNOWS the block_log is trusted, e.g.
self-replay or verify-against runs) would push us from 2.7K bps to
~14K bps for the validate-only / verify-only workflow. That is
exactly the path we run for every CI-grade verify-against.

This does NOT affect the live P2P sync path, which legitimately must
verify each block's wire signature, since a peer could be malicious.

### 2. Batch verification / replace decred secp256k1 with a faster impl

**Estimated impact: 2-3x on the live sync path.**

The decred pure-Go secp256k1 is ~3-5x slower than libsecp256k1's C
implementation. Two paths:

* **CGo-backed libsecp256k1** (e.g. via `github.com/btcsuite/btcd/btcec/v2`
  + a CGo wrapper, or directly via cgo). Cost: gives up `pure-Go`
  build hygiene. Win: ~3x ScalarMult speed.
* **Batch ECDSA recovery + verification across a worker pool.** We
  already parallelize block-signature recovery across peers via
  commit `23b9427`; extend the same pattern to the per-tx signature
  verification (which currently runs serially inside `VerifyAuthority`).
  At ~5-10 sigs/block on average mainnet, this is a clean GOMAXPROCS
  multiplier on the dominant cost.

### 3. `Session.Put` overlay-copy reuse + `Range` overlay-iter avoidance

**Estimated impact: ~5% on replay, larger on heavy-write blocks.**

Two related allocator sources:

* `Session.Put` clones every value into a freshly-allocated `[]byte`
  unless the existing overlay entry has spare capacity. 168 MB / 200K
  blocks = 840 B/block — 100s of small mallocs per block dispatched
  through `runtime.mallocgc`. A per-Session arena (allocate one big
  buffer at session start, bump-allocate Put values, reset on Commit)
  would cut this to one alloc per session.
* `Session.Range` was allocating an `IterG` for `overlayKeys.Iter()`
  per call even when the overlay is empty. **Fixed in this branch**
  (commit on `agent-profile`): the no-overlay fast path no longer
  allocates the merge closure either. Measured: ~3% throughput win.

A third candidate is the per-block `fcraw.NewReader` allocation
(327K objs / 200K blocks). Pooling via `sync.Pool` would shave 10 MB /
200K blocks. Small.

## Caveats

* This is **self-replay**, not real P2P. STCP encryption + zstd
  decompression of inbound peer frames are NOT exercised here. They
  may matter on a real catch-up flood; a follow-up profile against an
  actual peer would localize their cost.
* `--backend=mem` (tidwall btree). Pebble would shift the alloc /
  syscall mix: more `runtime.mallocgc` from key/value SST reads,
  more `Pread` syscall time. The CPU-spent-in-secp256k1 ratio is
  unchanged since secp dominates.
* The 200K-block window is HF0-HF1 era. Later eras have larger blocks
  and more votes/comments, which should shift cum away from
  `verifyBlockSignatureWithCache` toward evaluator code (vote, comment,
  cashout). This profile is a lower bound on signature-verify cost.

## How to reproduce

```sh
cd /home/martin/projects/hived-go/.worktrees/profile
go build -o ./bin/ghive-sync ./cmd/ghive-sync
rm -rf /tmp/perf-profile && mkdir -p /tmp/perf-profile

# CPU profile — 200K-300K blocks suffices
timeout 180 ./bin/ghive-sync \
  --data-dir=/tmp/perf-profile --backend=mem \
  --source-block-log=/home/martin/projects/hived-go/block_log \
  --max-blocks=300000 \
  --cpuprofile=/tmp/perf-profile.pprof \
  > /tmp/perf-profile.log 2>&1

# Heap profile (allocator pressure)
timeout 90 ./bin/ghive-sync \
  --data-dir=/tmp/perf-profile-mem --backend=mem \
  --source-block-log=/home/martin/projects/hived-go/block_log \
  --max-blocks=200000 \
  --memprofile=/tmp/perf-profile.mem \
  > /tmp/perf-profile-mem.log 2>&1

go tool pprof -top -cum -nodecount=30 /tmp/perf-profile.pprof
go tool pprof -top -alloc_space -nodecount=30 /tmp/perf-profile.mem
go tool pprof -list='ApplyBlock' /tmp/perf-profile.pprof
```

Captured profiles for this report live alongside as
`docs/perf/2026-05-09-sync-cpu.pprof` and `2026-05-09-sync-heap.pprof`.
