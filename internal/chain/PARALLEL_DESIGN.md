# Parallel block application (optimistic concurrency / Block-STM-lite)

Status: IN PROGRESS (scaffolding). Flag-gated, default OFF. The serial path is
the golden reference and MUST stay byte-identical; `Session.Spec == nil` is the
serial path and is never altered in behavior.

## Goal & expected payoff
Use the idle cores (replay is single-threaded; ~11 of 12 cores idle) to execute
a block's transactions in parallel. Amdahl cap for REPLAY is ~1.3-1.4x because
~58% of wall time is outside the per-op tx loop (eviction 27%, cashout 10%,
decompress 10%, sequential parse 11% — all block-boundary/level). The real
payoff is LIVE-SYNC, where signatures are verified (parallel secp256k1 already
exists) and there is no cold-tier eviction.

## Why optimistic (not static dependency analysis)
Evaluators access ALL state through ~162 package-level accessors in
internal/state/objects/*.go (`GetAccount/PutAccount/MutateDGPO/...`). They never
touch the typed stores or the Session overlay directly. That is the choke point:
instrument the accessors and we intercept every read/write without changing
evaluator or store code.

## Design: freeze → parallel speculate → serial validated commit
Per block, the tx loop is replaced (only when parallel mode is on) by:

1. FREEZE: committed state is read-only during the parallel phase (no committing
   thread mutates it). Each worker reads the real, block-start state directly
   (lock-free) and RECORDS what it read (read-set); WRITES go to a per-tx buffer
   (write-set). Known commutative accumulators are recorded as DELTAS and
   excluded from the read-set (see below).

2. PARALLEL: workers execute tx[0..N-1] each against its own `Session` whose
   `Spec` holds the read-set + write-set buffers. Same underlying `*Database`
   (shared stores, read-only in this phase).

3. SERIAL COMMIT in tx order: for tx[i]:
   - if tx is TAINTED (hit an accessor not yet speculative-instrumented) OR its
     read-set fails to validate against now-mutating state → RE-EXECUTE serially
     against the real state (the always-correct fallback);
   - else APPLY its buffered write-set + commutative deltas (cheap, no re-exec).
   Result == serial order exactly (RAW deps caught by read-set → re-exec; blind
   writes applied in tx order; commutative deltas summed in order).

## INCREMENTAL SAFETY (the key invariant)
`Spec.tainted` starts... every accessor that is NOT yet speculative-aware calls
`s.Spec.taint()` and falls back to reading/writing the REAL store (so a
speculatively-executing tx still produces *a* result, but is flagged for serial
re-exec at commit). Therefore:
- Increment 0 (all accessors un-instrumented): every tx taints → every tx
  re-executes serially at commit → BIT-IDENTICAL to serial, just with harness
  plumbing. This is the correctness baseline to validate first.
- Each later increment makes one accessor family (Account, then Comment, then
  CommentVote, Witness, DGPO-commutatives, ...) speculative-aware → those txs
  stop tainting → real parallelism grows.
At NO point can a missed accessor cause a wrong result: un-instrumented => taint
=> serial re-exec.

NOTE: because tainted txs write to the REAL store during the parallel phase,
the parallel phase can NOT actually run tainted txs concurrently with others
safely. So Increment 0's harness must, for tainted txs, NOT mutate real state in
the parallel phase — it must DISCARD speculative work for tainted txs and only
re-exec them serially at commit. Implementation: in parallel mode a worker runs
with a Spec that BUFFERS all writes (never touches real store) even for
un-instrumented accessors; un-instrumented accessors that READ go to the real
(frozen) store and RECORD nothing but set tainted=true; un-instrumented WRITES
buffer into a generic "opaque" sink and set tainted=true. At commit, tainted
txs are re-executed serially with Spec=nil. (So accessors NEVER write to the
real store while Spec!=nil; they either buffer or taint+buffer-opaque.)

## Commutative accumulators (delta handling)
Excluded from read-set; each tx records a delta; commit sums deltas in order:
- DGPO id counters: NextCommentID, NextAccountID, NextLimitOrderID,
  NextVestingDelegationExpirationID. CONSUMERS need the *assigned* sequential id
  (it lands in consensus state, e.g. comment_cashout key = (cashoutTime, id)).
  => these ops cannot be fully speculated; the id is assigned at commit in tx
  order and the dependent writes (comment.ID, schedule key) patched then. Treat
  id-consuming ops as a special commit step (or taint them initially).
- DGPO.TotalRewardShares2 (per comment vote): commutative sum.
- Witness.TotalVotesVests (per witness vote): commutative sum on the target.
- rc_pending_data (HF20+, per tx): commutative; PendingUsage/PendingCost/TxCount.
  (The profiled dense era 17-25M is PRE-HF20 so RC is not yet a factor.)

## Block-boundary work stays serial
Everything before/after the tx loop (header, applyStateDriftCorrections,
update*, processCommentCashouts, processFunds, EvictFrozen, rcFinalizeBlock,
...) runs serially exactly as today.

## Validation
- Increment 0: replay a range with parallel mode ON (all taint→serial) and diff
  the per-block DGPO / a full-state fingerprint against serial. Must be 0 diffs.
- Each increment: same range diff must stay 0.
- Golden reference: the serial 60M run (in progress) — any divergence vs hived
  is a pre-existing consensus bug, fixed independently of this work.

## Files
- internal/chain/parallel.go        — harness, scheduler, commit (NEW)
- internal/state/spec.go            — SpecContext, read-set, write buffers (NEW)
- internal/state/objects/*.go       — accessor speculative branches (incremental)
- internal/chain/database.go        — ApplyBlock tx-loop dispatch to harness

## Flag
`replay --parallel-apply[=N workers]` (default off). Also an ApplyOptions field
`ParallelApply int` (0 = serial).
