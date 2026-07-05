# Replay + snapshot performance audit — 2026-07-05

Goal: replay as fast as possible at the SAME memory ceiling; make snapshot
create/load much faster. Three code audits (apply hot path, snapshot
subsystem, blocklog+memory) + live pprof of a genesis→60M run.

## What was measured

Live gen60 run (persist cold, cap=1M, GOMEMLIMIT=24GiB), CPU profile at ~13M:
ApplyBlock 64% (vote+comment evaluators, map probes 12% flat), GC ~13%,
EvictFrozen+PersistCold.Set ~12%, tx parse ~10.6%, zstd-cgo decompress ~7.5%.
Read+decompress and parse are serial stalls on the apply goroutine.

Snapshot facts: 40M self-contained snapshot = 23GB, 99% comment_store.bin.
Old saves at 35/45M measured 516–592s; loads had NO timing at all. Writer was
strictly serial (one store after another), per-file fsync, in-line sha256;
loader serial with per-record alloc + full decode→re-encode round-trip for
every cold-bound record + incremental index rehash-doublings.

## Landed (commits 6a84a2a, 0a92c04)

1. **Prefetch pipeline** (`replay --prefetch=N`, default 8): reader goroutine
   runs BlockReuse (pread+zstd) ahead of apply through a recycled buffer ring.
   2M genesis: 39.5K → 81.7K blocks/s (**2.07x**), state bit-identical.
2. **COLD_HEAP_KEYS default ON** (opt out `=0`): default runs can no longer
   fall off the 4000→300 bps cold-index rehash disk-thrash cliff at 40M+.
3. **`--mem-limit-gib`**: debug.SetMemoryLimit wired in-process.
4. **Snapshot save parallelized** across mem.bin + all subsystem files, with
   per-file `wrote/loaded X MB in Ys (Z MB/s)` instrumentation. crc32c
   (HW-accelerated) replaces sha256 for NEW snapshots (manifest `hash_algo`);
   legacy sha256 manifests still verify. NOTE: old binaries cannot load
   crc32c snapshots (checksum mismatch) — new binaries load both.
5. **Snapshot load parallelized** (comment+vote stay sequential — shared cold
   tier is single-writer) + 1MiB buffered checksum tee.
6. **Raw-spill load**: cold-bound comment records are routed by a
   schema-tolerant peek (`peekCommentSpillRouting`) and written to the cold
   tier VERBATIM — no ReadComment/AppendComment round-trip, no per-record
   alloc. Old-vs-new loader on a real 3M cold snapshot + 100K continuation:
   byte-identical state. Covered by TestSelfContainedSnapshotRawSpillLoad.
7. **Index presizing**: persiststore keyIndex + mmapcold prim map pre-grow
   once from the snapshot record count (EnsureIndexCapacity/ensureCapacity)
   instead of log2(N) rehash-doublings.

Numbers at 6M scale (volatile mmap cold, cap 200K): save 3.5s → 1.6s,
load 16.6s → 9.3s; remaining load cost is the pre-HF17 by-root btree,
which does not exist for post-HF17 (late-chain) snapshots.

At 20M scale (11GB snapshot → persist cold, 12G cgroup) MEASURED WHILE the
stalled gen60 held 23GB RSS: old 475s ≈ new 479s → new+fadvise 411s. In that
regime the load is cgroup-reclaim/writeback-bound (~25MB/s), not CPU-bound —
the dirty-page flood from spilling ~10GB into the cold tier's MAP_SHARED
mmap dominates and masks the CPU-side wins. Re-measure on an idle box;
backlog: bulk cold-spill via pwrite/appender instead of mmap memcpy to give
the kernel writeback a sequential stream.

## Found while profiling: the 17–18M eviction-thrash stall

gen60 (cap=1M) stalled >25min between 18M and 19M at 98% CPU. Heap profile:
Go heap only 3.3GB (RSS 23GB is reclaimable file-backed cold-tier mmap).
CPU profile: ~50% GC-assist + ~92% of mutator inside CommentStore.EvictFrozen.
Cause: the spam-era unpaid comment window vastly exceeds cap=1M, so every
block runs the full forced-eviction scan (20K defers + forced evicts +
promote-backs). NOT a memory problem — a cap-too-small problem.

**Fix: run genesis replays with `--comment-resident-cap=4000000`** (the
validated bank-run setting) — that clears the 15–25M spam era. Note the
separate known issue past 40M: the unpaid working set exceeds RAM regardless
of cap (fat heap votes; the real fix is compact/co-located votes — see
project_perf_root_working_set_2026_07_01), so expect the late-chain slowdown
either way. Restart command for gen60:

```
systemd-run --user --unit=gen60 -p MemoryMax=30G -p MemorySwapMax=0 \
  -E GOMEMLIMIT=24GiB \
  -p StandardOutput=append:/home/martin/gen60.log -p StandardError=inherit \
  /home/martin/projects/hived-go/bin/ghive-tools replay --backend=mem \
  --pprof=localhost:6060 --cold-backend=persist --cold-dir=/home/martin/cold-genp \
  --comment-resident-cap=4000000 --snapshot-every=5000000 \
  --snapshot-dir=/home/martin/snaps_clean --snapshot-self-contained \
  --shutdown-snapshot-dir=/home/martin/snap60m_clean \
  --save-dir-snapshot-at=49590000 --save-dir-snapshot-to=/home/martin/snaps_clean \
  --count=60000000 --max-errors=1 --progress-every=1000000 \
  /home/martin/blocklog-60m/block_log
```
(wipe /home/martin/cold-genp first — fresh genesis run; restart
snapcleaner.sh afterwards; --save-dir-snapshot-at banks the wall-drill
snapshot at 49,590,000 since the run halts at the 49,596,514 wall.)

## Not landed (ranked backlog)

- **mem backend primary tables → hash map** (K/V btree point-ops are O(log n)
  cache-miss chains; mirror mmapcold's coldKeyIsPrimary split). Likely the
  next biggest apply win.
- **Commit dirtyKeys**: sort.Strings per block + per-key btree SetHint —
  bucket-group + bulk-load instead.
- **O(1) "nothing due" guards** for the remaining per-block sweeps
  (conversions, savings, governance expiry, recurrent transfers), pattern of
  DelayedVoteNoneDue.
- **Parse-ahead**: move tx/op deserialization into the prefetch stage
  (needs slab/interning care) — parse is ~10% of apply CPU.
- **Session.Put arena** for first-write-per-key value copies.
- **Cold-read batching**: pre-fault a block's cold comment/vote keys in
  parallel before the serial mutate phase (biggest lever for 40M+ cold-bound
  eras, where apply is single-threaded fault-bound).
- **--parallel-apply** is effectively inert (speculative whitelist =
  single-touch transfers only, pre-HF20 only).
- Vote store loads ALL votes resident on self-contained load (no spill path);
  fine at 211MB (40M era) but will grow.
- Stale tests (pre-existing failures, unrelated to this work):
  TestColdCommentEvictionEquivalence / TestColdVoteEvictionEquivalence expect
  the pre-two-tier byte-identical marshal; TestEvictFrozenPolicy expects
  strict age order (predates paid-preference deferral);
  TestTimeshareWitnessSchedulingHF25 fails at HEAD.
