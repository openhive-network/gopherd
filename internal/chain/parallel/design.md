# Tx-level intra-block parallelism: design

Tracking task #12 from the 2026-05-12 perf overhaul session.

## Goal

Apply non-conflicting transactions within a single block concurrently
across N goroutines (N = GOMAXPROCS). Today the replay loop is
~1.1× parallel (the runtime GC takes about 10% of CPU on a second
worker, but ApplyBlock itself is serial). 8M strict replay sits at
~43K bps vs C++'s ~74K average; parallelism is the only structural
lever left for closing that gap.

## Architecture overview

```
parse N txs ─► extract TouchSet[i] for each tx
            │
            ▼
build conflict graph: txs i, j conflict iff TouchSet[i].ConflictsWith(TouchSet[j])
            │
            ▼
greedy color the graph → batches of txs that can run in parallel
            │
            ▼
for each batch in order:
    parallel: for each tx in batch on its own goroutine:
        Snapshot(); apply ops; DiscardSavepoint() or Restore()
    barrier: wait
```

Two key invariants:

1. **Determinism.** Batches are processed in order. Within a batch, tx
   ordering doesn't matter (definition of "no conflict"), but the
   final state must equal the serial result. This holds when every
   pair of txs in the same batch has disjoint write sets.

2. **Snapshot per goroutine.** Each tx needs its own savepoint so a
   failure in tx-i doesn't observe partial writes from tx-j. The
   Session already supports nested savepoints; the parallel driver
   needs N independent Sessions sharing the same Database, or a
   per-tx session-shadow + a single barrier commit.

## Foundation already in this package

`touchset.go` (this commit) implements `TouchSet` + `Extract(t, op)`.
The conservative design ("anything not classified ⇒ Accounts + Funds +
Comment all touched ⇒ forces serial") guarantees correctness even
for unclassified ops; the unblocking is graceful.

`ConflictsWith` is the hot path of the conflict-graph build. With ~5
ops/tx and ≤30 txs/block, the per-block conflict-check budget is
~30² × 5 = ~4500 set-intersection ops per block. The inline
`AccountSet` (8-element fixed array) keeps each check at ~50 ns →
~250 µs per block of conflict-detection overhead vs ~10 ms of apply
budget at 100K bps. Negligible.

## What's NOT yet implemented

### 1. Thread-safe state stores (the hard part)

Every typed store (WitnessStore, AccountStore, CommentStore,
CommentVoteStore, DGPOStore, WitnessVoteStore, BlockSummaryStore,
TransactionStore, LimitOrderStore, ConvertRequestStore,
VestingWithdrawalScheduleStore, CommentCashoutScheduleStore) holds
maps + btrees + undoLogs that are not thread-safe.

Options:

a) **RWMutex per store.** Coarse but simple. Likely defeats the
   parallelism gain because every Mutate path takes the writer
   lock and serializes.

b) **Sharded stores.** Split each store into N shards keyed by some
   hash of the primary key. Each shard has its own lock. Goroutines
   mostly hit different shards. Implementation cost: medium. Memory
   cost: small (per-shard maps).

c) **Lock-free with concurrent writers.** Hard. Probably not worth
   it for this workload.

Recommendation: (b) with N=GOMAXPROCS shards, hash the account /
witness / commentID key into shard via a fast hash.

### 2. Conflict-graph batcher

After per-tx TouchSet extraction, build the conflict graph and
greedily color it. Pseudo-code:

```go
func planBatches(touch []TouchSet) [][]int { // returns slice of tx-id slices
    n := len(touch)
    color := make([]int, n)
    for i := 0; i < n; i++ {
        used := map[int]bool{}
        for j := 0; j < i; j++ {
            if touch[i].ConflictsWith(&touch[j]) {
                used[color[j]] = true
            }
        }
        c := 0
        for used[c] { c++ }
        color[i] = c
    }
    // Group by color, preserving within-color tx order for determinism.
    var batches [][]int
    for i, c := range color {
        for c >= len(batches) { batches = append(batches, nil) }
        batches[c] = append(batches[c], i)
    }
    return batches
}
```

### 3. Parallel apply driver

Restructure `ApplyBlock`'s per-tx loop into:

```go
for _, batch := range planBatches(touch) {
    if len(batch) == 1 {
        // Single-tx batch: apply on main goroutine (no goroutine
        // overhead).
        applyOneTx(session, &txRes[batch[0]])
        continue
    }
    var wg sync.WaitGroup
    for _, i := range batch {
        wg.Add(1)
        go func(i int) {
            defer wg.Done()
            applyOneTx(session, &txRes[i])
        }(i)
    }
    wg.Wait()
}
```

But `applyOneTx` needs each goroutine to operate on its own
Session-shadow so the snapshot/restore semantics don't race. This
is where the threadsafe-stores design comes back: either separate
Sessions or a Session that internally synchronizes per goroutine.

### 4. **Pessimistic touchset is unsound for ANY mainnet block**

Validated empirically 2026-05-12: wiring even the conservative
flag-based touchset (Account-set + Witness-set + CommentTouched +
FundsTouched, with AccountWitnessVote/Proxy and all market ops
opted into FundsTouched) into ApplyBlock as serial-but-batched
caused state divergence starting around block 3.4M.

Symptoms:
- "insufficient balance: clayop has 4838724 HBD, needs 4838728"
  (off by 4 sat — points at HBD interest accrual, which depends
  on per-balance-change timing, not just final balance)
- "limit_order_create: insufficient balance" + "limit_order_cancel
  not found" cascading after the first divergence

Root cause: there are indirect write paths I didn't capture:
- HBD interest accrual fires on **every** balance change and
  depends on the time since the previous balance-change of THIS
  account. Reordering balance-touching txs within a block
  produces slightly different interest payments → cumulative
  drift.
- Cashout schedule rescheduling reads root-comment state on
  every vote — reordering votes across same-thread comments
  changes the weighted-average cashout time.
- DGPO's `NextCommentID` is allocated at comment-create time;
  the assigned IDs depend on order even when the resulting state
  looks equivalent (downstream code keyed by ID would diverge).

**Conclusion:** A pessimistic flag-based touchset CANNOT be safe
for mainnet. Even the strictest opt-in to FundsTouched lets
non-conflicting flag classes (AccountUpdate vs Vote vs Transfer
with disjoint accounts) commute in ways that break HBD interest
accounting.

The right path forward is one of:

a) **Optimistic / speculative execution.** Run each tx against a
   shadow state in parallel; record exact reads + writes; after
   all txs finish, check whether any pair has read-write or
   write-write conflicts; if conflicts found, serially re-apply
   the offenders. This is what database OCC systems do. Requires
   per-goroutine session-shadows and a deterministic conflict
   detector. Conservative for correctness (false positives just
   cause serial fallback, never wrong state).

b) **Apply-and-trace touchset extraction.** Run each tx FIRST
   against a tracing wrapper that records every read + write key
   it actually touched. Use the trace as the touchset for the
   NEXT block at the same heights. Doesn't help the FIRST run.

c) **Per-account serialization with worker pools.** Drop the
   "batches" concept. Maintain a queue per account; tx_i goes
   into the queue of every account it touches; workers process
   queues in lockstep. Requires hashable account-only conflicts
   (no global resources like DGPO).

The pessimistic flag-based design (this commit + e50ef03)
remains a useful *primitive* for option (c) but NOT a sufficient
foundation on its own for safe parallel apply.

### 5. Per-block hooks remain serial

`processProducerReward`, `processCommentCashouts`,
`processConversions`, `processVestingWithdrawals`,
`updateWitnessSchedule`, `updateLastIrreversibleBlock`,
`processFunds`, etc. — these all run AFTER the tx loop and are
naturally serial. No change needed.

## Estimated win

Late blocks (3.5M+) average ~3-5 txs per block. With perfect
parallelism on a 4-core machine and ~30% of CPU in apply-Tx work
(rest is decompress + schedule + hooks), the upper bound is ~30%
faster on the apply step → ~10-15% overall throughput. So ~43K → ~50K
bps at 8M. Still short of C++'s 74K, but a real improvement.

The remaining ~50% gap to C++ requires either GC tuning (Go's
generational GC isn't as cache-friendly as C++'s explicit ownership)
or async I/O for block reads — neither is in scope here.

## Why I'm not implementing the rest in this session

Each of the three remaining pieces (thread-safe stores, batcher,
parallel driver) needs careful design, implementation, AND
correctness validation. A failed parallel-apply implementation
silently corrupts state (the strict-replay diff would catch it
eventually, but only after a multi-hour run).

The right next move: a dedicated work session that ports ONE typed
store to sharded locking (start with WitnessStore since it's the
most-mutated), validates serial replay still PASSes through 8M
blocks, then implements the batcher + parallel driver, and finally
validates parallel replay matches serial replay output exactly.

That's a 3-5 day project, not a chat-iteration project.
