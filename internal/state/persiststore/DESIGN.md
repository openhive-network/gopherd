# Persistent state (chainbase-in-Go): restart without replay

Goal: match hived/chainbase — the bulk of consensus state lives in mmap-backed
files so a CLEAN restart resumes at head with NO replay, while hot-path access
stays at ~RAM speed. Snapshots then go back to being a CASCADE-DEBUG tool (jump
back many blocks), not the restart mechanism.

## Why the current Go port can't restart
The hot tables (AccountStore, CommentStore, CommentVoteStore, WitnessStore, …)
are native Go maps — GC-managed, process-local pointers. They cannot be mmap'd
and remapped. Their only on-disk form is a snapshot, so every restart =
snapshot-load (+ replay of the gap since the snapshot). The cold tier
(internal/state/objects/mmapcold.go) is already a chainbase-style mmap table for
evicted comments/votes — this project generalizes that to ALL state.

## chainbase recap (what we're emulating)
One mmap'd `shared_memory.bin` holds boost multi-index containers whose internal
pointers are file-relative offsets (offset_ptr) → position-independent. Mutations
are direct writes to mapped pages (RAM speed); OS page cache persists + handles
>RAM paging. Clean shutdown flushes + sets a clean flag → restart re-mmaps and
resumes instantly. Crash → dirty flag → must --replay-blockchain. (So hived also
replays after an UNCLEAN kill; only clean shutdowns resume instantly.)

## Go approach: mmap data + reloadable index
Go maps can't be mmap'd, so we split:
- DATA in an mmap file, offset-addressed, records SELF-DESCRIBING
  `[keyLen uvarint][key][valLen uvarint][val]` so the index can always be
  rebuilt by a forward scan (crash-robust; fixes the cold tier's non-scannable
  layout).
- INDEX (key→(offset,len)) in a Go map, rebuilt on open: from a persisted
  sidecar on CLEAN shutdown (fast), or by a forward scan of the data file on a
  DIRTY/crash open (slower, still no replay). "Instant remap" (index also in
  mmap via an offset-addressed hashmap) is a later optimization.
- A slot ALLOCATOR over the data file: append at writeOff; a size-classed
  free-list reclaims slots freed by delete / size-changing update (bounds the
  dead-space growth the cold tier currently suffers).
- UNDO log for tx rollback: record (key, prevOffset, prevLen, prevBytes-or-tomb)
  before each mutation; RestoreToPoint replays in reverse. Mirrors the existing
  typed-store undo contract so the chain layer is unchanged.
- CLEAN/DIRTY flag in a header page: set dirty on first write after open, clear
  + flush (msync + sidecar write) on Close. Open with dirty set → rebuild index
  by scan.

## Phases (each independently testable; replay stays on the current architecture
## until a phase is validated bit-identical)
1. **persiststore primitive** (THIS phase): the mmap data segment + self-
   describing records + append/free-list allocator + in-RAM index + flush
   (sidecar) + open (sidecar-or-scan rebuild). Test: write N → Close → Open →
   all N present, byte-identical, no replay. Plus delete/realloc/free-list.
2. **In-place mutation + undo log**, matching the typed-store SnapshotPoint /
   RestoreToPoint / CommitPoint contract.
3. **AccountStore on persiststore** (smallest big table, ~1.4M, no cold tier).
   Validate: replay bit-identical to current; kill+reopen resumes at head.
4. **CommentStore / CommentVoteStore** — replace mmapcold + the resident maps
   with one persiststore each (resident = OS page cache of hot pages; no manual
   evict/promote). Removes the eviction machinery entirely.
5. **Witness / singletons / schedules / RC** tables.
6. **Wire restart**: SIGTERM → flush all tables → exit clean; startup → open all
   tables (resume at head from the dgpo). Snapshots become cascade-debug only.

## Non-goals / risks
- Determinism: record bytes use the existing Append*/Read* codecs unchanged, so
  state is identical regardless of storage. Iteration order for consensus scans
  must stay sorted (index keeps a sorted view or sorts on demand).
- This is consensus-critical: every phase gates on a bit-identical replay diff
  vs the current engine before it ships to the real run.
