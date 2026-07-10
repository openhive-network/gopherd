# Fork switching / reorg handling — design

## Problem
The warm-start head-follower reaches the live frontier but can't sustain tracking
through tip micro-forks. Two failure faces observed:
- `validate_block N: previous_id mismatch` — node applied an orphan block N-1, the
  canonical N builds on a different N-1; node drops N forever, head wedges.
- Peer ban: `You offered me a block that I have deemed to be invalid` — node applied
  a minority-fork tip block, gossiped it, peers reject+drop it → peers=0 → wedge.

The P2P applyLoop is strictly linear (`head+1`), has no fork model, and drops any
`previous_id`-mismatch block. State application is write-forward: once `Session.Commit`
flushes to pebble there is no undo.

## Constraints from the architecture map
- Writes reach pebble ONLY at `Session.Commit` (session.go:444). SoftCommit keeps the
  overlay in RAM but `commitAll()` drops subsystem undo logs each block.
- Subsystem undo entries are value-type before-images (reusable across blocks if not
  dropped), captured only while `UndoEnabled` (savepoint depth>0). Per-block hooks run
  at depth 0 (no capture) — must force `UndoEnabled` for the reversible window.
- Backend has no undo primitive. Cold-tier eviction is write-forward + age-based and
  runs with UndoEnabled=false → the single biggest correctness risk.
- Full-state snapshot is tens-of-GB — NOT viable per reorg.

## Design: reverse-batch reversible window (opt-in, live mode only)
Replay/bulk-sync path is UNCHANGED (uses existing per-block Commit) → zero risk to the
validated genesis→head replay. Reversibility engages only when the node is at/near the
tip (a `ReversibleMode` flag on the chain.Database), so the reverse-batch cost (an extra
before-image capture per dirty key) is only paid for the handful of live tip blocks.

### State layer (internal/state)
1. `ReversibleMode` flag. When on, ApplyBlock keeps `UndoEnabled=true` for the whole
   block (so per-block hooks are logged) and does NOT drop subsystem undo at block end.
2. Per committed block, before applying the pebble batch, capture a **reverse batch**:
   the before-image (current backend value, or "absent") of every dirty key. Commit
   forward to pebble as normal, then push `{reverseBatch, subsystemMarkers, blockID,
   prevID, num}` onto a bounded reversible stack.
3. `PopBlock()`: apply the reverse batch to the backend (restore before-images) +
   `RestoreToPoint` every subsystem to its marker + pop the stack. Restores exact state.
4. `FinalizeBelow(libNum)`: for blocks at/below LIB, drop their reverse batches and
   compact the subsystem undo logs from the front (drop finalized entries, reindex
   remaining markers). Bounds memory. maxReversibleDepth default e.g. 64 (> any realistic
   micro-fork; forced-finalize if exceeded).

### Cold-tier hazard
Gate `EvictFrozen` so it never evicts a comment/vote created within the reversible window
(newer than LIB). Age-order already keeps newest last, so this is a cheap guard: pass the
LIB block-num and skip candidates newer than it. (Reversible window is tiny for a tip
follower, so RAM cost is negligible.)

### Fork database (internal/p2p/sync)
`forkDB`: in-memory DAG of recent block headers keyed by block ID, each with `prev`, `num`,
raw bytes. Tracks branches. Longest (highest num; ties → first-seen) branch = preferred head.

### Reorg driver (applyLoop)
On a block whose `prev` is a KNOWN block that isn't current head:
- add to forkDB. If its branch is now strictly longer than current head's branch:
  - find common ancestor (walk both back by ID).
  - `PopBlock()` from head down to the common ancestor.
  - apply the new branch's blocks forward. If any fails, roll back (re-apply original
    branch from the popped blocks, which we retain) and drop the bad branch.
Linear `head+1` extension stays the fast path (no forkDB work).

### Gossip gate
Only re-broadcast a block once it is on the node's current best chain (head). Don't gossip
a block that lost a fork / isn't the head — this stops the "offered invalid block" peer ban.

## Phases
1. State reversible primitive: ReversibleMode + reverse-batch + PopBlock + FinalizeBelow.
   Unit test: apply K blocks in reversible mode, PopBlock all, assert state == pre-apply
   (byte-identical via a state fingerprint).
2. Eviction LIB gate.
3. forkDB + common-ancestor.
4. Reorg driver in applyLoop + gossip gate.
5. Integration test (synthetic 1-2 block reorg) + live tip validation.
