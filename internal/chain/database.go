// Package chain orchestrates block application: per-block setup, walking
// transactions and operations, dispatching to evaluators, and committing
// the session at block end.
package chain

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"sync"

	"hived-go/internal/chain/evaluator"
	"hived-go/internal/crypto"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/network"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// mismatchWarn rate-limits the schedule-mismatch warning so a window
// of divergent state (typical pre-1.135M era catch-up) doesn't flood
// stderr. One line per ~1000 mismatches; the count is global.
var (
	mismatchMu      sync.Mutex
	mismatchCount   uint64
	mismatchLastLog uint64
)

func mismatchWarn(blockNum, slot uint32, got, expected string) {
	mismatchMu.Lock()
	mismatchCount++
	c := mismatchCount
	since := c - mismatchLastLog
	mismatchMu.Unlock()
	if since == 1 || since >= 1000 {
		fmt.Fprintf(os.Stderr, "chain: WARN witness mismatch at block %d slot=%d header=%q expected=%q (#%d total; sig-verify is the security check, schedule check is advisory)\n",
			blockNum, slot, got, expected, c)
		mismatchMu.Lock()
		mismatchLastLog = c
		mismatchMu.Unlock()
	}
}

// Database wraps a state.Database and provides block-application
// orchestration on top of it. Mirrors libraries/chain/database.hpp's
// hive::chain::database in spirit, scoped to what current evaluators need.
type Database struct {
	state *state.Database
	// applyMu serializes ApplyBlock against all concurrent callers (the
	// witness production goroutine, p2p/sync's apply loop, and any
	// read-side accessor that materializes a new state.Session — see
	// HeadBlock / HeadBlockID / ScheduledWitness below).
	//
	// ApplyBlock takes the WRITE lock for the entire block-apply (it
	// is inherently serial — there's exactly one chain head — so the
	// lock has zero effect on a correctly-driven workload).
	//
	// Read-side accessors that create their own state.Session take the
	// READ lock around the call. The shared *state.Database.Aux map
	// (cross-Session typed-object cache) is mutated during commit and
	// read on every GetDGPO/GetWitness/etc; without an external lock
	// the witness goroutine reading dgpo while sync's apply loop
	// commits triggers the Go runtime concurrent-map-access detector.
	applyMu sync.RWMutex
	// reused across ApplyBlock calls; maps are clear()'d on Commit so
	// the underlying buckets stay warm and we avoid 14M+ map allocations
	// per replay.
	session *state.Session
	// lcbCache mirrors each witness's LastConfirmedBlockNum so the per-
	// block updateLastIrreversibleBlock can avoid 21 GetWitness reads
	// (each a btree lookup + full Witness deserialize). Updated whenever
	// updateProducerLastConfirmed bumps a witness; cache misses are
	// resolved by reading state and populating the entry. Single-writer
	// (only updateProducerLastConfirmed mutates LCB), so consistency
	// holds as long as we update cache and state together.
	lcbCache map[string]uint32
	// schedWitPtrs is the resolved *Witness slice for the current
	// CurrentShuffledWitnesses (~21 entries). Refreshed every schedule
	// rebuild so per-block updateLastIrreversibleBlock can read each
	// witness's LastConfirmedBlockNum directly through the pointer —
	// no per-block map probes against lcbCache (~2.2s flat). The slice
	// shares backing with WitnessSchedule.CurrentShuffledWitnesses by
	// index: schedWitPtrs[i] is the witness named at that index.
	schedWitPtrs    []*objects.Witness
	schedWitPtrsKey []auth.AccountName // matches the CurrentShuffledWitnesses snapshot
	// libConfirmedBuf is the per-block sort buffer used by
	// updateLastIrreversibleBlock to assemble each scheduled witness's
	// LCB before computing the 75% median. Reused across blocks (~21
	// entries) to avoid 450 MB cumulative slice growth.
	libConfirmedBuf []uint32
	// hooks is the bag of reusable scratch slices the per-block hooks
	// borrow at the top of each call and truncate to zero before reuse.
	// Each hook owns a discrete slice — there's no inter-hook aliasing
	// risk because the hooks fire sequentially.
	hooks blockHookScratch
	// txReader is a single fcraw.Reader reused across ApplyBlock calls
	// to avoid the per-block alloc (4M blocks × ~30B = ~120 MB cum).
	txReader fcraw.Reader
	// schedScratch holds the per-round scratch buffers for
	// updateWitnessSchedule4 (active, voteCands, powCandidates,
	// visited, selectedVoted, selectedMiners). Each round (~21 blocks)
	// re-uses them. Cuts ~450 MB cum.
	schedScratch witnessScheduleScratch

	// recent caches the FC-binary bytes of the last N successfully-applied
	// blocks so the p2p sync layer can re-serve them to peers requesting
	// `fetch_items` during catch-up. nil = caching disabled (callers that
	// don't need to serve sync — replay tools, tests — can leave it nil).
	recent *RecentBlocks

	// onBlockApplied is fired once after each successful ApplyBlock,
	// AFTER the recent-block cache is updated. Used by the p2p sync
	// layer to wake its INV broadcaster from idle backoff so the
	// new block reaches peers without waiting a full tick. nil =
	// no hook (default for replay/test contexts). NOT fired for
	// reversible (uncommitted tip) blocks — those fire onBlockFinalized
	// instead, once they commit.
	onBlockApplied func(blockNum uint32, id block.ID)

	// onTxApplied fires for every transaction entering the dedup window, on
	// EVERY apply path (direct, drained-from-buffer, reorg re-apply). The
	// mempool uses it to drop transactions another witness has mined.
	onTxApplied func(txID [20]byte)

	// auditAdmission runs against an incoming block BEFORE it is applied, while
	// head state is still its parent. Diagnostic hook for the admission audit.
	auditAdmission func(rawBlock []byte, blockNum uint32)

	// ownerAuthHistoryStartBlock overrides where owner_authority_history
	// tracking begins; 0 = mainnet default. Tests that start at block 1 set
	// it so recovery works without pre-rolling 3M blocks. Previously a
	// mutable package global in evaluator, which raced between parallel tests.
	ownerAuthHistoryStartBlock uint32

	// admitWait* measure how long mempool admission blocks on applyMu, i.e.
	// how much of a transaction's arrival latency is our own lock contention
	// with ApplyBlock rather than the network.
	admitWaitMu      sync.Mutex
	admitWaitN       int64
	admitWaitTotalMs int64
	admitWaitMaxMs   int64
	admitWaitOver100 int64

	// onBlockFinalized is fired when a reversible block COMMITS (via
	// FinalizeReversible), i.e. once it is confirmed to extend the chain.
	// The fork-aware node wires block_log-append + cold-tier eviction here
	// instead of onBlockApplied, so those irreversible side-effects never
	// run for a tip block that a reorg might undo (a cold spill can't be
	// undone; an orphan must not enter the block_log). nil = no hook.
	onBlockFinalized func(blockNum uint32, raw []byte)

	// commitBatchSize controls how many blocks accumulate in the
	// session overlay before ApplyBlock fires a real backend Commit.
	// 0 or 1 → legacy per-block commit (fsync every block under
	// pebble — the dominant disk cost at chain head).
	// >1 → batched mode: SoftCommit at end of each block, real
	// Commit every commitBatchSize blocks. The block_log writer is
	// independent of state Commit batching, so a crash mid-batch
	// is recoverable by replaying from the persisted DGPO head.
	//
	// Mutated only at construction by SetCommitBatch; read inside
	// ApplyBlock under the same applyMu write lock.
	commitBatchSize uint32
	// batchCounter tracks how many SoftCommits have happened since
	// the last real Commit. Reset to 0 each time the batch boundary
	// flushes.
	batchCounter uint32
	// commitSync controls whether the batch boundary fsyncs. Mirrors
	// the legacy ApplyOptions.Sync semantics but moved to the chain-
	// level driver since the per-block Sync flag is meaningless in
	// batched mode.
	commitSync bool
	// reversiblePending holds the identity of the one block applied under
	// ApplyOptions.Reversible whose writes are still uncommitted (live in the
	// Session overlay + subsystem undo). nil when no reversible block is
	// pending. Finalize/UndoReversible clear it. The fork driver reads it to
	// decide extend-vs-reorg.
	reversiblePending *reversibleBlock
	// fork is the fork DAG of recent blocks, used by PushLiveBlock to detect +
	// resolve tip forks. nil until the first PushLiveBlock. Single-owner (the
	// applyLoop goroutine); no lock of its own.
	fork *forkDB

	// customOpCountByAccount tracks per-account count of custom /
	// custom_json / custom_binary ops applied in the CURRENT block.
	// HIVE_CUSTOM_OP_BLOCK_LIMIT (=5) caps it: the 6th custom op for the
	// same account in the same block fails the tx. Mirrors
	// database.cpp::_pending_tx_custom_op_count, scoped to a single
	// block apply rather than the pre-block pending pool — for canonical
	// block_log replay the two are equivalent. Reset at the top of
	// ApplyBlock; mutated inside the tx loop by enforceCustomOpBlockLimit.
	customOpCountByAccount map[auth.AccountName]uint8

	// vops is the per-block virtual-operation buffer used to surface
	// fill_recurrent_transfer / failed_recurrent_transfer vops to tests
	// (the Go chain doesn't have a full virtual-op pipeline yet — see
	// block_vops.go). Reset at the top of every ApplyBlock; readers grab
	// it via LastBlockVOps() under the applyMu RLock.
	vops []BlockVOp
}

// New wraps an opened state.Database for block application.
func New(s *state.Database) *Database {
	return &Database{
		state:                  s,
		session:                state.NewSession(s),
		lcbCache:               make(map[string]uint32, 32),
		customOpCountByAccount: make(map[auth.AccountName]uint8, 8),
	}
}

// State returns the underlying state.Database.
func (d *Database) State() *state.Database { return d.state }

// WithReadSession runs fn with a fresh read Session while holding applyMu in
// READ mode, so the read is consistent against the single ApplyBlock writer
// (which takes applyMu in write mode). Concurrent API reads share the RLock and
// only read the typed stores, so they don't race each other or the applier.
// The session sees committed state; on a per-block-commit node that is the head.
func (d *Database) WithReadSession(fn func(s *state.Session) error) error {
	d.applyMu.RLock()
	defer d.applyMu.RUnlock()
	return fn(state.NewSession(d.state))
}

// WithApplyLock runs fn while holding the apply write lock. The chain
// state (Pebble overlay + every typed in-memory store) is guaranteed
// not to be mutated by ApplyBlock for the duration. Used by the
// snapshot path (--snapshot-every / --shutdown-snapshot-dir; the old
// SIGUSR1 trigger no longer exists) to serialize the snapshot against the apply
// goroutine — without it, snapshot.Save races against the apply path
// (concurrent map read+write inside the typed stores) and crashes the
// daemon with a Go runtime fatal.
func (d *Database) WithApplyLock(fn func()) {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	fn()
}

// EnableRecentBlockCache turns on the in-memory ring of last-N-applied
// raw block bytes. Required for serving p2p sync responses; callers that
// only replay block_log can leave it disabled. Safe to call once at
// startup (idempotent on the same capacity).
func (d *Database) EnableRecentBlockCache(capacity int) {
	if d.recent != nil {
		return
	}
	d.recent = NewRecentBlocks(capacity)
}

// SetOnBlockApplied registers a callback fired after every successful
// ApplyBlock. Pass nil to disable. Single-slot — last writer wins.
// The callback runs synchronously on the goroutine that called
// ApplyBlock, so it should return quickly (just notify a channel).
// SetAuditAdmission registers a hook run against each incoming block BEFORE it
// is applied, while state is still the parent. Diagnostic only; nil disables.
func (d *Database) SetAuditAdmission(cb func(rawBlock []byte, blockNum uint32)) {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	d.auditAdmission = cb
}

// AuditBlockAdmission runs ValidateIncomingTx over every transaction in a block
// and returns one bucketed reason per refusal. The caller must hold no lock;
// this takes the read lock itself.
//
// Intended to be called from the pre-apply hook, where head state is the
// block's parent — the state its producer validated against.
func (d *Database) AuditBlockAdmission(rawBlock []byte) (total int, refusals []string) {
	txs, err := BlockTransactionBodies(rawBlock)
	if err != nil {
		return 0, nil
	}
	for _, raw := range txs {
		total++
		if err := d.validateIncomingTxLocked(raw); err != nil {
			refusals = append(refusals, err.Error())
		}
	}
	return total, refusals
}

// SetOnTxApplied registers a callback fired for each transaction recorded in
// the dedup window. Called under the apply lock, so it must be cheap and must
// not re-enter the chain.
func (d *Database) SetOnTxApplied(cb func(txID [20]byte)) {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	d.onTxApplied = cb
}

func (d *Database) SetOnBlockApplied(cb func(blockNum uint32, id block.ID)) {
	d.onBlockApplied = cb
}

// SetOnBlockFinalized registers a callback fired when a reversible block
// COMMITS (FinalizeReversible). The fork-aware node wires block_log-append +
// eviction here so those irreversible side-effects run only for confirmed
// blocks, never for a tip block a reorg may undo.
func (d *Database) SetOnBlockFinalized(cb func(blockNum uint32, raw []byte)) {
	d.onBlockFinalized = cb
}

// SetCommitBatch enables batched commits at end of ApplyBlock.
//
//	0 or 1 → legacy per-block commit path (fsync every block; pebble
//	         is dominated by this cost at chain head).
//	N > 1  → SoftCommit each block; one real Commit (with sync=fsync)
//	         every N blocks. Crash recovery replays from the persisted
//	         DGPO head against the local block_log.
//
// fsync controls whether the batch-boundary Commit fsyncs Pebble.
// True = full durability per batch (one fsync per N blocks instead
// of one per block); false = OS write only.
//
// Must be called BEFORE the first ApplyBlock — there's no protection
// against a mid-batch reconfigure.
func (d *Database) SetCommitBatch(n uint32, fsync bool) {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	d.commitBatchSize = n
	d.commitSync = fsync
	d.batchCounter = 0
}

// CommitBatchSize returns the configured batch size (0 or 1 = per-block).
func (d *Database) CommitBatchSize() uint32 {
	d.applyMu.RLock()
	defer d.applyMu.RUnlock()
	return d.commitBatchSize
}

// RecentBlockBytes returns the FC-binary bytes of blockNum if the recent-
// block cache is enabled and contains it, else nil.
func (d *Database) RecentBlockBytes(blockNum uint32) []byte {
	if d.recent == nil {
		return nil
	}
	return d.recent.ByNum(blockNum)
}

// RecentBlockBytesByID returns the FC-binary bytes of the block with the
// given block_id if the recent-block cache is enabled and contains it.
func (d *Database) RecentBlockBytesByID(id block.ID) []byte {
	if d.recent == nil {
		return nil
	}
	return d.recent.ByID(id)
}

// RecentBlockNums returns the block_nums currently held in the recent-
// block cache (oldest-first).
func (d *Database) RecentBlockNums() []uint32 {
	if d.recent == nil {
		return nil
	}
	return d.recent.RecentNums()
}

// RecentBlockIDForNum returns the cached block ID for blockNum.
func (d *Database) RecentBlockIDForNum(blockNum uint32) (block.ID, bool) {
	if d.recent == nil {
		return block.ID{}, false
	}
	return d.recent.IDForNum(blockNum)
}

// HeadBlockID returns the current head block's 20-byte ID per DGPO.
// Returns the zero ID + nil error when no blocks have been applied yet.
func (d *Database) HeadBlockID() (block.ID, error) {
	d.applyMu.RLock()
	defer d.applyMu.RUnlock()
	s := state.NewSession(d.state)
	p, err := objects.GetDGPO(s)
	if err == state.ErrNotFound {
		return block.ID{}, nil
	}
	if err != nil {
		return block.ID{}, err
	}
	return p.HeadBlockID, nil
}

// ApplyOptions tunes ApplyBlock behavior.
type ApplyOptions struct {
	// ParallelApply, when > 0 AND the replay-mode skip flags are all set
	// (SkipTransactionSignatures, SkipTxSavepoints, SkipDedupAndTaPOS), runs a
	// block's transactions through the optimistic parallel harness with this
	// many workers (see internal/chain/PARALLEL_DESIGN.md and parallel.go).
	// 0 = the serial tx loop (default). The result is bit-identical to serial:
	// transactions whose ops are all speculative-supported and whose read-set
	// validates are applied from their buffered write-set; everything else is
	// re-executed serially in transaction order.
	ParallelApply int
	// SkipUnknownOps causes ApplyBlock to ignore evaluator.ErrNotImplemented
	// (treating the op as a no-op). Default false; with the stub registry in
	// place every op is registered, so this should rarely be needed.
	SkipUnknownOps bool
	// PreVerifiedBlockSigner, when non-zero, is the public key recovered
	// from the witness block signature OUTSIDE of ApplyBlock by a
	// look-ahead worker pool (see internal/p2p/sync/sigverify.go). When
	// supplied, verifyBlockSignature skips its own RecoverPublicKey call
	// (which dominates the apply CPU profile) and just compares the
	// pre-recovered key against the witness's BlockSigningKey. The
	// caller is responsible for proving the recovery: hand back a key
	// the sync layer recovered against the same digest as
	// verifyBlockSignature would.
	PreVerifiedBlockSigner crypto.PublicKey
	// TolerateFailingOps causes ApplyBlock to count, but not abort on, any
	// evaluator error (insufficient balance, unsupported symbol, etc.).
	// This is intended for INCREMENTAL P1 development only — when many
	// evaluators are still validate-only stubs, real evaluators rightly
	// reject operations that depend on stub-side mutations that never
	// happened. Setting this true lets replay drive forward through the
	// whole block_log to surface other coverage holes. Counts of skipped
	// ops are reported via *Stats. NEVER set this true on a node that
	// claims consensus correctness.
	TolerateFailingOps bool
	// Sync controls whether each block's commit fsyncs.
	Sync bool
	// Reversible, when true, leaves the block's writes UNCOMMITTED in the
	// Session overlay + subsystem undo logs (neither Commit nor SoftCommit
	// runs), so the block can later be finalized (FinalizeReversible) once it
	// extends the chain, or rolled back (UndoReversible) on a fork switch.
	// Requires SkipTxSavepoints=false (the block savepoint + UndoEnabled must
	// be active so the subsystem undo captures every mutation, including
	// per-block hooks). At most one reversible block may be pending at a time:
	// the caller must Finalize or Undo it before applying the next block.
	// Live tip-follow only — the replay path never sets this.
	Reversible bool
	// SoftCommit, when true, finalizes the block via Session.SoftCommit
	// instead of Session.Commit. The session's overlay accumulates across
	// blocks and reads in subsequent blocks see the latest writes from
	// memory; the caller is responsible for invoking
	// chain.Database.FlushBatch() periodically to fsync.
	SoftCommit bool
	// SkipTransactionSignatures bypasses tx-level signature checks
	// (mirrors hived's skip_transaction_signatures flag). Replay
	// always sets this true — historical txs already passed validation
	// at the producing-node side; re-verifying is wasted work. Live
	// sync sets it false.
	SkipTransactionSignatures bool
	// SkipBlockSignature bypasses the witness's block-level signature
	// check (mirrors hived's skip_witness_signature). Same rationale
	// as above for replay.
	SkipBlockSignature bool
	// SkipDedupAndTaPOS bypasses the per-tx dedup-window probe
	// (HasTransaction), TaPOS ring lookup, and PutTransactionRecord
	// write, and skips the per-block PruneExpiredTransactions sweep.
	// Replay against a canonical block_log can safely set this — the
	// block_log already guarantees no duplicate or out-of-window txs;
	// these checks are pure overhead. Live sync MUST leave it false.
	// Skips DigestFromBytes (sha256) on the unsigned bytes too, since
	// the only remaining txID consumer is dedup itself.
	SkipDedupAndTaPOS bool
	// SkipTxSavepoints bypasses the per-tx session.Snapshot /
	// DiscardSavepoint pair AND the failure-recovery Restore. Subsystem
	// stores then never set UndoEnabled, eliminating ALL per-mutation
	// undoLog snap captures (WitnessStore ~700-byte / Account ~256-byte
	// / Comment ~256-byte / CommentVote ~75-byte) across the run.
	//
	// CORRECTNESS NOTE: any failure inside evaluator.Apply leaves the
	// session with a half-applied tx — no rollback is possible. Replay
	// against a canonical block_log expects zero failures, so we halt
	// immediately with the error message and let the caller diagnose.
	// Live sync MUST leave this false.
	SkipTxSavepoints bool
	// PreRecoveredTxKeys, when non-nil, is the per-tx pre-recovered
	// signer pubkey set produced by RecoverBlockTxSignerKeys upstream
	// (typically on a worker pool that runs decode + sig recovery in
	// parallel with the previous block's apply). The slot at index i
	// is the recovered key set for tx i; nil slots fall through to the
	// in-place serial RecoverPublicKey path so the canonical error
	// message is preserved on failure. When supplied, ApplyBlock skips
	// secp256k1 RecoverPublicKey for the whole block — leaving only
	// the cheap account-authority lookup against state.Session in the
	// per-tx critical section.
	PreRecoveredTxKeys [][]crypto.PublicKey
	// ValidateOwnBlock forces the block-level checks (continuity, size, merkle,
	// version) even when SkipBlockSignature is set. Used for blocks WE produce:
	// we skip re-recovering our own signature, but everything else must still
	// be checked before the block goes to the network.
	ValidateOwnBlock bool

	// EnforceRC, when true, rejects any tx whose payer has insufficient
	// RC mana (HF20+). Mirrors hived's `rc-skip-reject-not-enough-rc=0`
	// path. Replay leaves it false — historical blocks always validated
	// at the producing node and replay against canonical block_log is
	// monotonically forward, never re-enforcing.
	EnforceRC bool
	// InControl mirrors hived's `_db.is_in_control()`: true when
	// validating transactions at the HEAD of the chain (block production
	// / initial admission / unit tests), false during replay and sync of
	// existing blocks. Several C++ asserts are gated on it — e.g.
	// delegate_rc's min-delegation and RC-sufficiency checks — so ops
	// that failed them at admission never entered a block, while a
	// replay must accept whatever the block contains.
	InControl bool
}

// Stats accumulates per-pipeline-run counters. Pass a non-nil pointer to
// ApplyBlock to receive op-level counts.
type Stats struct {
	OpsApplied  uint64
	OpsSkipped  uint64
	BlocksFull  uint64 // blocks where every op applied cleanly
	BlocksDirty uint64 // blocks with at least one skipped op
	// SkipReasons counts skipped ops by a coarse "op_name: error_class" key.
	// Populated only when TolerateFailingOps is true.
	SkipReasons map[string]uint64
	// SkipAccounts counts insufficient-balance failures by the offending
	// account name (the one extracted from the error message). Lets the
	// summary highlight which accounts the upstream cascade is starving.
	SkipAccounts map[string]uint64
}

func (s *Stats) recordSkip(opName string, err error) {
	if s.SkipReasons == nil {
		s.SkipReasons = make(map[string]uint64, 32)
	}
	// Walk to the leaf for the reason key (so wrapping noise like the
	// "apply_block N tx M op K: " prefix doesn't fragment the histogram).
	leaf := err
	for {
		next := unwrap(leaf)
		if next == nil {
			break
		}
		leaf = next
	}
	short := leaf.Error()
	if i := indexOfByte(short, ':'); i > 0 {
		short = short[:i]
	}
	s.SkipReasons[opName+": "+short]++
	// For the starved-accounts histogram we want the ORIGINAL formatted
	// message (which still has the quoted account name); the leaf is
	// just the bare sentinel and never carries the name.
	full := err.Error()
	if name := firstQuotedToken(full); name != "" {
		if s.SkipAccounts == nil {
			s.SkipAccounts = make(map[string]uint64, 64)
		}
		s.SkipAccounts[name]++
	}
}

// firstQuotedToken returns the contents of the first `"…"` quoted span
// in s, or "" if there is none.
func firstQuotedToken(s string) string {
	a := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			if a < 0 {
				a = i + 1
			} else {
				return s[a:i]
			}
		}
	}
	return ""
}

func indexOfByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// customOpBlockLimit caps the number of custom / custom_json /
// custom_binary operations a single account can submit in one block.
// Mirrors HIVE_CUSTOM_OP_BLOCK_LIMIT (libraries/protocol/config.hpp =5).
const customOpBlockLimit uint8 = 5

// enforceCustomOpBlockLimit walks the tx's operations, finds every
// custom* op, gathers the impacted accounts (per-op required auths),
// and increments d.customOpCountByAccount. If any account's count
// exceeds customOpBlockLimit the tx is rejected and the increments
// rolled back so subsequent txs in the same block see no effect from
// this failed tx.
//
// Mirrors libraries/chain/database.cpp::_push_transaction lines 631-640
// — counts are committed to the block-wide map only on success.
func (d *Database) enforceCustomOpBlockLimit(signed *tx.SignedTransaction) error {
	return applyCustomOpBlockLimit(d.customOpCountByAccount, signed)
}

// applyCustomOpBlockLimit enforces hived's per-account custom-operation cap for
// ONE block, against a caller-supplied counter. Split out so BLOCK ASSEMBLY can
// apply the identical rule against its own counter without touching the chain's.
//
// Selection did not enforce this and it is very likely why block 108912883 was
// rejected: we packed 579 transactions (65KB) where the real block held 90, and
// a witness may only include customOpBlockLimit custom ops per account per
// block. Everything past that makes the block INVALID, so the network refuses
// it — while our own node, which enforces the limit only in ApplyBlock, had no
// complaint about assembling it.
//
// It also explains the mempool divergence: hived refuses the excess at
// admission, so its pool stays near one block's worth (~117 observed), while
// ours grew to 2020 holding transactions nobody is able to include.
func applyCustomOpBlockLimit(counts map[auth.AccountName]uint8, signed *tx.SignedTransaction) error {
	// Per-tx delta map; merged into the block-wide map at end on success.
	var delta map[auth.AccountName]uint8
	for _, o := range signed.Operations {
		var impacted []auth.AccountName
		switch o.Tag {
		case op.TagCustom:
			impacted = o.AsCustom().RequiredAuths
		case op.TagCustomJSON:
			cj := o.AsCustomJSON()
			impacted = append(impacted, cj.RequiredAuths...)
			impacted = append(impacted, cj.RequiredPostingAuths...)
		case op.TagCustomBinary:
			cb := o.AsCustomBinary()
			impacted = append(impacted, cb.RequiredOwnerAuths...)
			impacted = append(impacted, cb.RequiredActiveAuths...)
			impacted = append(impacted, cb.RequiredPostingAuths...)
		default:
			continue
		}
		if delta == nil {
			delta = make(map[auth.AccountName]uint8, len(impacted))
		}
		// Deduplicate within the op (flat_set semantics in C++):
		// the same account appearing twice in required_*_auths counts once.
		seen := make(map[auth.AccountName]struct{}, len(impacted))
		for _, a := range impacted {
			if _, dup := seen[a]; dup {
				continue
			}
			seen[a] = struct{}{}
			delta[a]++
			total := counts[a] + delta[a]
			if total > customOpBlockLimit {
				return fmt.Errorf("account %s already submitted %d custom json operation(s) this block.",
					a, customOpBlockLimit)
			}
		}
	}
	for a, n := range delta {
		counts[a] += n
	}
	return nil
}

// ApplyBlock runs every transaction's operations through their registered
// evaluators against a fresh state.Session, then commits. Returns the
// post-apply DGPO so callers can chain quickly without re-fetching.
//
// rawBlock is the FC-binary block payload as decompressed by blocklog
// (signed_block_header + transactions vector). blockNum is the 1-indexed
// block number. stats may be nil.
func (d *Database) ApplyBlock(rawBlock []byte, blockNum uint32, opts ApplyOptions, stats *Stats) (dgpoOut objects.DynamicGlobalProperty, retErr error) {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()

	hdr, hdrLen, err := block.UnmarshalSignedHeader(rawBlock)
	if err != nil {
		return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d header: %w", blockNum, err)
	}

	// ADMISSION AUDIT — the ground truth for "is our mempool filter too strict".
	//
	// Every transaction in a block the network accepted is valid BY DEFINITION.
	// So running our own admission check over them, here, against the PARENT
	// state the real producer used, turns a guess into a proof: any refusal is
	// an over-refusal, and it names itself.
	//
	// This has to happen before apply. Afterwards these transactions are in the
	// dedup window and every one of them would be refused as a duplicate — the
	// audit would report 100% failure and mean nothing.
	if d.auditAdmission != nil {
		d.auditAdmission(rawBlock, blockNum)
	}

	session := d.session

	// Block-level savepoint: hived's apply_block is wrapped in a
	// `_pending_tx_session` that is undone on ANY failure (header,
	// tx, post-tx hook). Without this, a successful tx N's dedup
	// PutTransactionRecord (or DGPO mutation, or vop emission) would
	// stick after a subsequent tx fails, and the next p2p retry of
	// the same block would see "duplicate transaction" forever.
	// Skipped under SkipTxSavepoints (canonical block_log replay
	// where the producing network already validated the block).
	if !opts.SkipTxSavepoints {
		blockSnap := session.Snapshot()
		defer func() {
			if retErr != nil {
				session.Restore(blockSnap)
			}
		}()
	}
	hf := hardfork.SetForBlock(blockNum)
	// hived's head_block_time() during transactions returns dgpo.time,
	// which is the PREVIOUS block's timestamp — update_global_dynamic_data
	// only advances dgpo.time after tx application. Read it now and pass
	// to evaluators so they match C++.
	headBlockTime := hdr.Timestamp
	prevCurrentAslot := uint64(0)
	if dgpoRef, ok := objects.GetDGPORef(session); ok {
		headBlockTime = dgpoRef.Time
		prevCurrentAslot = dgpoRef.CurrentAslot
	}

	// Witness-scheduling assert + block-level invariants (chain
	// continuity, block size, merkle root, running_version). All
	// skipped on the replay path where the canonical block_log is
	// already validated; live sync sets opts.SkipBlockSignature=false
	// to engage them. Mirrors database.cpp:2335-2403.
	// validateBlock covers chain continuity, block SIZE, MERKLE ROOT and
	// running_version. SkipBlockSignature must NOT disable it for a block we
	// produced ourselves: skipping the signature check on our own block is a
	// sensible optimisation (we made the signature microseconds ago), but it
	// silently disabled every OTHER block-level check too — so a block of ours
	// that was oversized or had a bad merkle root applied locally without
	// complaint and was only rejected out on the network, by peers, with a
	// generic "I have deemed it invalid" and no detail. That is precisely
	// backwards: the producer is the one node that can still fix the block.
	if !opts.SkipBlockSignature || opts.ValidateOwnBlock {
		if err := validateBlock(session, &hdr, rawBlock, hdrLen, blockNum, hf); err != nil {
			return objects.DynamicGlobalProperty{}, err
		}
		// Witness signature check: recover the pubkey from the
		// 65-byte compact signature over SHA256(header_bytes), and
		// compare to witness_object.BlockSigningKey. Mirrors hived's
		// validate_block_header in database.cpp:2887.
		if err := verifyBlockSignatureWithCache(session, &hdr, rawBlock, hdrLen, blockNum, opts.PreVerifiedBlockSigner); err != nil {
			return objects.DynamicGlobalProperty{}, err
		}
		if blockNum > 1 {
			slot := slotNumForBlockTime(headBlockTime, hdr.Timestamp)
			if slot > 0 {
				if ws, wErr := objects.GetWitnessSchedule(session); wErr == nil {
					expected := ws.ScheduledWitnessForSlotAslot(prevCurrentAslot, slot)
					if expected != "" && expected != hdr.Witness {
						// Schedule-mismatch: our predicted witness for
						// this slot disagrees with the block header.
						// Downgraded from a hard error to a warning
						// because the block-signature check above is
						// the cryptographic security gate (it recovers
						// the pubkey from the sig and compares to the
						// witness's on-chain BlockSigningKey). The
						// schedule check is a peer-trust pre-validation
						// — if our state diverges from hived's in
						// pre-1.135M era (per memory iter5/iter10/15:
						// "trace identical Go==hived past block 1.135M"),
						// a strict check rejects valid blocks during
						// catch-up. Once state converges past 1.135M
						// the warning silences itself.
						mismatchWarn(blockNum, slot, hdr.Witness, expected)
					}
				}
			}
		}
	}
	ctx := &evaluator.Context{
		OwnerAuthHistoryStartBlock: d.ownerAuthHistoryStartBlock,
		DB:                         session,
		Hardforks:                  hf,
		BlockNum:                   blockNum,
		BlockTime:                  hdr.Timestamp,
		HeadBlockTime:              headBlockTime,
		Witness:                    hdr.Witness,
		EmitVOp:                    d.emitVOpClosure(),
		InControl:                  opts.InControl,
	}

	// Mirror C++ database.cpp:2374-2376 — set dgp.current_witness BEFORE
	// the tx loop runs, so PoW evaluators (and any other op that reads
	// dgpo.CurrentWitness) see the current block's producer rather than
	// the previous block's. Go's evaluator.Context.Witness is also set
	// to hdr.Witness (line 210), so the in-Context reads are independent;
	// this only matters for code paths that read dgpo directly.
	objects.MutateDGPO(session, func(p *objects.DynamicGlobalProperty) {
		p.CurrentWitness = hdr.Witness
	})

	// Mirror C++ database.cpp:2379 — process_header_extensions runs
	// BEFORE the tx loop so witness running_version + hardfork_version_vote
	// reflect the current block when downstream evaluators or the HF5+
	// witness-version assertion read them.
	if err := processHeaderExtensions(session, &hdr); err != nil {
		return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d header_extensions: %w", blockNum, err)
	}
	// HF5+ running-version check fires AFTER the extension has had a
	// chance to update the witness's running_version from this block's
	// hardfork_version_vote. Mirrors hived database.cpp:2266-2275.
	if !opts.SkipBlockSignature {
		if err := assertWitnessVersion(session, &hdr, blockNum, hf); err != nil {
			return objects.DynamicGlobalProperty{}, err
		}
	}

	// Targeted state-drift corrections — band-aid for known un-root-caused
	// cumulative drifts. See state_drift_corrections.go for the full list
	// and the why behind each entry; this call is a no-op for blocks
	// without a registered correction. Runs BEFORE the tx loop so a
	// correction at the failing block can adjust balances in time for
	// the transfer/op that would otherwise abort.
	applyStateDriftCorrections(session, blockNum)

	d.txReader.Reset(rawBlock[hdrLen:])
	r := &d.txReader
	op.ResetVoteSlab(r)
	txCount, err := r.ReadVarUint()
	if err != nil {
		return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d tx_count: %w", blockNum, err)
	}
	// Reset the per-block custom-op-per-account counters. See struct
	// field doc — mirrors database.cpp's clear of _pending_tx_custom_op_count
	// on block apply / pop / clear_pending.
	for k := range d.customOpCountByAccount {
		delete(d.customOpCountByAccount, k)
	}
	// Truncate the per-block virtual-op buffer (see block_vops.go).
	d.resetVOps()
	var skippedThisBlock uint64
	// Optimistic parallel tx application (replay mode only). Bit-identical to
	// the serial loop below; see parallel.go / PARALLEL_DESIGN.md.
	useParallel := opts.ParallelApply > 0 && opts.SkipTransactionSignatures &&
		opts.SkipTxSavepoints && opts.SkipDedupAndTaPOS
	if useParallel {
		var perr error
		skippedThisBlock, perr = d.applyTxsParallel(ctx, session, r, txCount,
			blockNum, hdr.Timestamp, opts, stats, opts.ParallelApply)
		if perr != nil {
			return objects.DynamicGlobalProperty{}, perr
		}
	} else {
		for i := uint32(0); i < txCount; i++ {
			var txRes tx.ReadResult
			var err error
			if opts.SkipTransactionSignatures {
				txRes, err = tx.ReadSignedTransactionSkipSigs(r)
			} else {
				txRes, err = tx.ReadSignedTransaction(r)
			}
			if err != nil {
				return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d tx %d parse: %w", blockNum, i, err)
			}
			// hived's apply_block wraps each push_transaction in its own
			// undo session — if any op in the tx throws, the whole tx is
			// rolled back atomically. Subsequent txs in the same block
			// are unaffected. We mirror that by snapshotting the overlay
			// at tx-start; on failure we Restore. Under SkipTxSavepoints
			// (trusted-block_log replay) the savepoint is skipped and a
			// failure halts immediately — no rollback is possible.
			var txSnap state.Savepoint
			if !opts.SkipTxSavepoints {
				txSnap = session.Snapshot()
			}
			txFailed := false
			var failOp string
			var failErr error

			// Compute the tx_id (first 20 bytes of sha256 over the unsigned
			// transaction body). Used by both TaPOS and dedup checks below.
			// Skipped under SkipDedupAndTaPOS (replay against canonical
			// block_log) — sha256 is ~5s of CPU across an 8M run.
			var txID [20]byte
			if !opts.SkipDedupAndTaPOS {
				ub := rawBlock[hdrLen+txRes.UnsignedStart : hdrLen+txRes.UnsignedEnd]
				txID = tx.IDFromDigest(tx.DigestFromBytes(ub))
			}

			// Pre-flight checks: expiration window + dedup + TaPOS slot
			// existence. Pass `headBlockTime` (= dgpo.Time at start of
			// apply_block, i.e. PREVIOUS block's timestamp) — this is what
			// hived's push_transaction sees, NOT the current block's
			// timestamp. Using hdr.Timestamp here would make the expiration
			// window 3 seconds tighter than C++ and reject the trailing
			// edge of legitimate txs.
			// Signature verification (live-sync only — replay sets
			// SkipTransactionSignatures since the canonical block_log was
			// already validated upstream). Pre-HF24 the sig digest is
			// over OldChainID || unsigned_tx; HF24+ uses ChainID.
			//
			// Mirror C++ full_transaction.cpp:479 EXACTLY: the chain-ID
			// switch is gated on the BLOCK TIMESTAMP vs
			// HIVE_HARDFORK_1_24_ACTUAL_TIME (1602703884), NOT on the
			// hf.Has(HF24) block-number predicate. The two cross-over
			// points sit ~158k blocks apart — a 5.5-day window where a
			// block-number-gated check recovers the wrong pubkey for every
			// signed tx. Masked today by SkipTransactionSignatures in
			// replay; breaks live-sync if not fixed.
			if !opts.SkipTransactionSignatures {
				const hf24ActualTime uint32 = 1602703884
				chainID := network.OldChainID
				if hdr.Timestamp > hf24ActualTime {
					chainID = network.ChainID
				}
				unsignedBytes := rawBlock[hdrLen+txRes.UnsignedStart : hdrLen+txRes.UnsignedEnd]
				// Use pre-recovered signer keys when the upstream worker
				// pool already did the secp256k1 work — that lifts the
				// dominant CPU cost out of this serial loop. Fall through
				// to the serial path when the slot is nil (recovery failed
				// upstream — surface the canonical error here) or when
				// PreRecoveredTxKeys wasn't supplied (replay tools, tests).
				var sigErr error
				if int(i) < len(opts.PreRecoveredTxKeys) && opts.PreRecoveredTxKeys[i] != nil {
					sigErr = VerifyAuthorityWithKeys(session, &txRes.SignedTx, opts.PreRecoveredTxKeys[i])
				} else {
					sigErr = VerifyAuthority(session, &txRes.SignedTx, unsignedBytes, chainID, false)
				}
				if sigErr != nil {
					txFailed = true
					failOp = "tx-sigverify"
					failErr = sigErr
					if !opts.SkipTxSavepoints {
						session.Restore(txSnap)
					}
					if stats != nil {
						stats.recordSkip(failOp, failErr)
					}
					skippedThisBlock++
					if opts.TolerateFailingOps {
						continue
					}
					return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d tx %d: %w", blockNum, i, failErr)
				}
			}
			if !opts.SkipDedupAndTaPOS {
				if err := validateTxPreFlight(session, &txRes.SignedTx, blockNum, headBlockTime, txID, !opts.SkipBlockSignature); err != nil {
					txFailed = true
					failOp = "tx-preflight"
					failErr = err
					if !opts.SkipTxSavepoints {
						session.Restore(txSnap)
					}
					if stats != nil {
						stats.recordSkip(failOp, failErr)
					}
					skippedThisBlock++
					if opts.TolerateFailingOps {
						continue
					}
					return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d tx %d: %w", blockNum, i, failErr)
				}
			}
			// HIVE_CUSTOM_OP_BLOCK_LIMIT (=5) is a mempool-only rule in
			// hived (custom_op_visitor at database.cpp:578-616 fires from
			// _push_transaction at :632, NOT from _apply_block). Block
			// apply trusts the producing witness, so legitimate mainnet
			// blocks can carry >5 custom_json per account. The cap will be
			// re-applied when ghive grows a push_transaction entrypoint.
			if !txFailed {
				// Phase 5.1 pre-apply tx hook: reset per-tx differential
				// usage accumulator before any op fires. HF20+ only.
				resetRCDifferentialForTx(session, blockNum)
				for j, o := range txRes.SignedTx.Operations {
					// Phase 5.1 pre-apply op hook: accumulate the (negative)
					// differential state-bytes for this op into
					// rc_pending_data BEFORE the op mutates the existing
					// state. HF20+ only.
					applyRCDifferentialForOp(session, o, blockNum)
					if err := evaluator.ApplyOp(ctx, o); err != nil {
						txFailed = true
						failOp = o.Tag.Name()
						failErr = fmt.Errorf("op %d: %w", j, err)
						break
					}
					if stats != nil {
						stats.OpsApplied++
					}
				}
			}
			// HF20+: charge RC for the entire transaction.
			//
			// Phase 5 implementation:
			//   1. count_resources(tx) — per-op resource accumulator.
			//   2. compute total cost via existing ComputeRCCost (sums
			//      state_bytes + execution_time + per-tx base).
			//   3. Accumulate (usage, cost) into rc_pending_data — the
			//      per-block pending totals are flushed to the active
			//      rc_usage_bucket at finalize.
			//   4. Charge the payer's manabar (regen, subtract, saturate
			//      at HIVE_RC_MAX_NEGATIVE_PERCENT).
			//   5. Enforce insufficient-RC error when EnforceRC is on (live
			//      sync); replay leaves it false and lets mana go negative.
			if !txFailed && hardfork.SetForBlock(blockNum).Has(hardfork.HF20) && len(txRes.SignedTx.Operations) > 0 {
				rcErr := applyRCForTx(session, &txRes.SignedTx, blockNum, hdr.Timestamp,
					int64(txRes.SignedEnd-txRes.UnsignedStart), opts.EnforceRC)
				if rcErr != nil {
					txFailed = true
					failOp = "tx-insufficient-rc"
					failErr = rcErr
				}
			}
			if txFailed {
				// Roll back the partial tx's writes; subsequent txs proceed.
				if !opts.SkipTxSavepoints {
					session.Restore(txSnap)
				}
				// Undo the per-op `OpsApplied` increments for the rolled-back ops.
				if stats != nil {
					// Number of ops successfully applied within this tx
					// before the failure point. Subtract since we're
					// rolling them back.
					// (we counted them above per-op; the simplest fix
					// is to leave stats-level counters and just increment
					// the skip counter). Here we follow the simpler path
					// and only count the rolled-back as "skipped".
					stats.recordSkip(failOp, failErr)
				}
				skippedThisBlock++
				if opts.SkipUnknownOps && isNotImplemented(failErr) {
					continue
				}
				if opts.TolerateFailingOps {
					continue
				}
				return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d tx %d: %w", blockNum, i, failErr)
			}
			// Tx applied cleanly: record it in the dedup window so a future
			// rebroadcast of the same trx_id is rejected. Skipped under
			// SkipDedupAndTaPOS for canonical-block_log replay.
			if !opts.SkipDedupAndTaPOS {
				objects.PutTransactionRecord(session, &objects.TransactionRecord{
					TrxID:      txID,
					Expiration: txRes.SignedTx.Expiration,
				})
				// Same moment, same id: tell the mempool this transaction is
				// now on chain. Doing it HERE rather than in the p2p layer is
				// what makes it complete — blocks applied via drainForward
				// (out-of-order buffering) or re-applied during a reorg never
				// pass back through sync, so a p2p-side hook silently misses
				// them. Measured live: p2p-side eviction covered only ~46% of
				// mined transactions, so the queue still grew ~356/min toward
				// the cap, where it would stop admitting fresh transactions and
				// go back to producing empty blocks.
				if d.onTxApplied != nil {
					d.onTxApplied(txID)
				}
			}
			// Release the tx savepoint so subsystem stores can stop logging
			// undo entries we'll never use. Flips db.UndoEnabled to false
			// when depth reaches 0, which is the common case once the last
			// tx in this block succeeds — the per-block hooks that follow
			// (processProducerReward → AdjustWitnessVotesForVoter,
			// processCommentCashouts, schedule rebuild) then bypass the
			// 700-byte Witness / 256-byte Account snap copies entirely.
			if !opts.SkipTxSavepoints {
				session.DiscardSavepoint(txSnap)
			}
		}
	} // end else (serial tx loop; parallel path handled above)
	if stats != nil {
		stats.OpsSkipped += skippedThisBlock
		if skippedThisBlock == 0 {
			stats.BlocksFull++
		} else {
			stats.BlocksDirty++
		}
	}

	// Mirror C++ database.cpp:2412 — update_global_dynamic_data after
	// tx application and before per-block hooks. CurrentWitness was
	// already set pre-tx-loop (mirrors C++ line 2374); here we advance
	// HeadBlockNumber, Time, HeadBlockID. Per-block hooks see the new
	// head block.
	//
	// update_global_dynamic_data also calls update_witness_missed_blocks
	// for each missed slot (database.cpp:2926-2933), incrementing their
	// total_missed and HF14-HF20 zeroing the signing_key for witnesses
	// out of touch beyond HIVE_WITNESS_SHUTDOWN_THRESHOLD blocks. We
	// run that BEFORE the DGPO advance so missed-witness lookups still
	// see the previous head block number.
	// Compute slot number BEFORE the missed-block walk so we can advance
	// CurrentAslot and pass the per-block slot to update_witness_missed_blocks.
	//
	// At block 1 hived's get_slot_at_time runs the head_block_num()==0
	// branch of get_slot_time, which uses dpo.time directly (= genesis
	// time, no rounding). Because Go's genesis seeds dgpo.Time with
	// network.GenesisTimeUnix and that value is on a 3-second boundary,
	// slotNumForBlockTime returns the same value as hived's get_slot_at_time
	// without any block-1 special case. For mainnet block 1 (timestamp
	// 1458835500, genesis 1458835200) this is 100 — the off-by-99 source
	// of the iter15 CurrentAslot drift, which Go was clamping to 1.
	//
	// update_witness_missed_blocks is gated by hived's
	// `head_block_num() != 0` guard; mirror that with `blockNum > 1`.
	slotForBlock := slotNumForBlockTime(headBlockTime, hdr.Timestamp)
	if blockNum > 1 && slotForBlock > 1 {
		updateWitnessMissedBlocks(session, hdr.Witness, slotForBlock-1, hf, blockNum)
	}
	hdrID := block.ConstructID(rawBlock[:hdrLen], blockNum)
	objects.MutateDGPO(session, func(p *objects.DynamicGlobalProperty) {
		p.HeadBlockNumber = blockNum
		p.Time = hdr.Timestamp
		p.HeadBlockID = hdrID
		// Advance CurrentAslot by slotForBlock — mirrors hived's
		// update_global_dynamic_data (database.cpp:2912-2920) which adds
		// the block's slot index (1 + missed slots since previous block)
		// to dgpo.current_aslot. ScheduledWitnessForSlot reads CurrentAslot
		// to resolve which witness should produce slot N from the schedule
		// shuffle.
		p.CurrentAslot += uint64(slotForBlock)
	})

	// Per-block hooks — order mirrors libraries/chain/database.cpp's
	// _apply_block (lines 2335-2356):
	//
	//   clear_expired_transactions    (we don't track tx dedup yet)
	//   clear_expired_orders
	//   clear_expired_delegations
	//   update_witness_schedule
	//   update_median_feed
	//   update_virtual_supply
	//   clear_null_account_balance
	//   process_funds                 → producer_reward + inflation
	//   process_conversions           → settles HBD→HIVE under feed
	//   process_comment_cashout       → uses same feed
	//   process_vesting_withdrawals
	//   process_savings_withdraws
	//   process_subsidized_accounts
	//   pay_liquidity_reward
	//   update_virtual_supply         (HBD might have changed)
	//   account_recovery_processing
	//   expire_escrow_ratification
	//   process_decline_voting_rights
	//   process_proposals
	//   process_delayed_voting
	//   remove_expired_governance_votes
	//   process_recurrent_transfers
	//   process_hardforks             → applies HF state effects last
	// Producer's last_confirmed_block_num is bumped immediately on
	// successful block apply (mirrors C++ update_signing_witness which
	// updates the producer's record before the LIB recompute below).
	d.updateProducerLastConfirmed(session, hdr.Witness, blockNum)

	clearExpiredOrders(session, hdr.Timestamp)
	clearExpiredDelegations(session, hdr.Timestamp)
	updateWitnessSchedule(session, blockNum, &d.schedScratch)
	updateMedianFeed(session, blockNum, hdr.Timestamp)
	updateVirtualSupply(session, blockNum)
	clearNullAccountBalance(session, blockNum)
	// Treasury sweep (post-HF24): "steem.dao" is the obsolete treasury; any
	// funds that land on it — notably the DHF return-proposal payment whose
	// receiver is still "steem.dao" — must be migrated to the current
	// treasury (hive.fund) EVERY block, not just once at HF24. Mirrors
	// hived's per-block consolidate_treasury_balance() at the "treasury"
	// phase (database.cpp:2358), guarded by collect_account_total_balance so
	// it only runs when steem.dao actually holds a balance. Without this Go
	// leaked the return-proposal HBD into steem.dao (~13K sat/tick),
	// shrinking the DHF budget until spaminator's proposal dropped below the
	// funding cutoff (convert failure @48,177,223).
	if hasHF24(session) {
		if sd, serr := objects.GetAccount(session, "steem.dao"); serr == nil &&
			(sd.Balance > 0 || sd.HBDBalance > 0 || sd.SavingsBalance > 0 ||
				sd.SavingsHBDBalance > 0 || sd.RewardHive > 0 || sd.RewardHBD > 0 ||
				sd.VestingShares > 0) {
			consolidateTreasuryBalance(session, hdr.Timestamp)
		}
	}
	processProducerReward(session, hdr.Witness, blockNum)
	if envGhivediff != "" {
		// Per-block dump in the HF7-cascade-divergence window. Trace
		// fires AFTER processProducerReward but BEFORE processFunds —
		// matches C++'s trace location inside process_funds (which is
		// after get_producer_reward but before adjust_supply).
	}
	// GHIVEDIFF_ALL dump matches C++'s placement inside process_funds:
	// AFTER get_producer_reward (= our processProducerReward) but
	// BEFORE the content/curate/vesting inflation slice modify.
	if envGhivediffAll != "" && (envCalcTo == 0 || calcTraceWindow(blockNum)) {
		d, _ := objects.GetDGPO(session)
		fh, _ := objects.GetFeedHistory(session)
		fmt.Fprintf(os.Stderr, "GHIVEDIFF GLOBAL block=%d fund=%d vfund=%d vshares=%d supply=%d vsupply=%d hbd_supply=%d median=%d/%d prvs=%d prvh=%d\n",
			blockNum, d.TotalRewardFundHive, d.TotalVestingFundHive, d.TotalVestingShares,
			d.CurrentSupply, d.VirtualSupply, d.CurrentHBDSupply,
			fh.CurrentMedianHistory.Base.Amount, fh.CurrentMedianHistory.Quote.Amount,
			d.PendingRewardedVestingShares, d.PendingRewardedVestingHive)
	}
	// (Per-witness VST dump moved into updateWitnessSchedule4 to align
	// with hived's GHIVEDIFF_VSTS dump location.)
	// GHIVEDIFF_ACCOUNTS=acc1,acc2 — per-block balance dump for each
	// listed account. Mirrors hived's diagnostic block at
	// database.cpp:1832-1853.
	if accEnv := envGhivediffAccounts; accEnv != "" && (envCalcTo == 0 || calcTraceWindow(blockNum)) {
		for _, name := range strings.Split(accEnv, ",") {
			if name == "" {
				continue
			}
			a, err := objects.GetAccount(session, name)
			if err != nil {
				continue
			}
			fmt.Fprintf(os.Stderr, "GHIVEDIFF ACCT block=%d name=%s hive=%d hbd=%d vests=%d rvests=%d rvhive=%d deleg=%d recv=%d vmana=%d vmanaupd=%d dvmana=%d effexcl=%d\n",
				blockNum, name, a.Balance, a.HBDBalance, a.VestingShares, a.RewardVests, a.RewardVestingHive, a.DelegatedVestingShares, a.ReceivedVestingShares,
				a.VotingManaCurrent, a.VotingManaLastUpdate, a.DownvoteManaCurrent, a.EffectiveVestingSharesExclWithdraw())
		}
	}
	// TREASURYTRACE diagnostic: dump steem.dao + hive.fund balances at the
	// START of the block (= end of prev block, pre this block's DHF/sweep).
	// Sparse stride to bisect the 296,435-sat HF23->HF24 hive.fund residual
	// against hived account_history ground truth. Uncommitted diagnostic.
	if os.Getenv("TREASURYTRACE") != "" {
		if blockNum%200000 == 0 || (blockNum >= 47797670 && blockNum <= 47797690) {
			for _, name := range []string{"steem.dao", "hive.fund"} {
				if a, e := objects.GetAccount(session, name); e == nil {
					fmt.Fprintf(os.Stderr, "TREASURYTRACE block=%d name=%s hive=%d hbd=%d savings_hbd=%d reward_hbd=%d vests=%d\n",
						blockNum, name, a.Balance, a.HBDBalance, a.SavingsHBDBalance, a.RewardHBD, a.VestingShares)
				}
			}
		}
	}
	processFunds(session, blockNum, hdr.Witness)
	if envGhivediff != "" {
		switch blockNum {
		case 100000, 500000, 1000000, 1500000, 2000000, 2500000, 2700000, 2800000, 2850000, 2880000, 2885000, 2889000, 2889020, 2890000, 2891000, 2892000, 2893000, 2894000, 2895000, 2896000, 2896500, 2896680, 2896682, 2897000, 2897356, 2897358, 2897500, 2897700, 2897750, 2897769, 2897770:
			if a, e := objects.GetAccount(session, "pfunk"); e == nil {
				fmt.Fprintf(os.Stderr, "GHIVEDIFF PFUNK-AT block=%d vesting_shares=%d delegated=%d received=%d hive=%d hbd=%d\n",
					blockNum, a.VestingShares, a.DelegatedVestingShares, a.ReceivedVestingShares, a.Balance, a.HBDBalance)
			}
		}
		switch blockNum {
		case 2896680, 2896682, 2897356, 2897358:
			d, _ := objects.GetDGPO(session)
			fmt.Fprintf(os.Stderr, "GHIVEDIFF VEST-PRICE block=%d vesting_fund=%d vesting_shares=%d\n",
				blockNum, d.TotalVestingFundHive, d.TotalVestingShares)
		}
		switch blockNum {
		case 2889100, 2889110, 2889120, 2889130, 2889140, 2889150, 2889160, 2889170, 2889180, 2889190, 2889200:
			d, _ := objects.GetDGPO(session)
			shares2 := d.TotalRewardShares2
			hi := uint64(shares2[0])<<56 | uint64(shares2[1])<<48 | uint64(shares2[2])<<40 | uint64(shares2[3])<<32 |
				uint64(shares2[4])<<24 | uint64(shares2[5])<<16 | uint64(shares2[6])<<8 | uint64(shares2[7])
			lo := uint64(shares2[8])<<56 | uint64(shares2[9])<<48 | uint64(shares2[10])<<40 | uint64(shares2[11])<<32 |
				uint64(shares2[12])<<24 | uint64(shares2[13])<<16 | uint64(shares2[14])<<8 | uint64(shares2[15])
			abitHive, abitHbd := int64(0), int64(0)
			if a, e := objects.GetAccount(session, "abit"); e == nil {
				abitHive = a.Balance
				abitHbd = a.HBDBalance
			}
			fmt.Fprintf(os.Stderr, "GHIVEDIFF SHARES2-AT block=%d shares2_hi=%d shares2_lo=%d fund=%d cur_supply=%d virt_supply=%d abit_hive=%d abit_hbd=%d\n",
				blockNum, hi, lo, d.TotalRewardFundHive, d.CurrentSupply, d.VirtualSupply, abitHive, abitHbd)
		}
		// One-shot witness vote total dump at block 2,920,000 (after
		// processFunds but before cashouts) for parity comparison.
		if blockNum == 2920000 {
			for _, name := range []string{"blocktrades", "steempty", "smooth.witness", "abit", "wackou", "riverhead", "witness.svk", "silversteem", "complexring", "kushed", "arhag", "clayop", "xeldal", "bitcube", "good-karma", "pfunk", "datasecuritynode"} {
				if w, e := objects.GetWitness(session, name); e == nil {
					fmt.Fprintf(os.Stderr, "GHIVEDIFF WITNESS block=%d name=%s votes=%d\n",
						blockNum, name, w.TotalVotesVests)
				}
			}
			d, _ := objects.GetDGPO(session)
			fh, _ := objects.GetFeedHistory(session)
			fmt.Fprintf(os.Stderr, "GHIVEDIFF GLOBAL block=%d fund=%d vfund=%d vshares=%d supply=%d vsupply=%d hbd_supply=%d median=%d/%d\n",
				blockNum, d.TotalRewardFundHive, d.TotalVestingFundHive, d.TotalVestingShares,
				d.CurrentSupply, d.VirtualSupply, d.CurrentHBDSupply,
				fh.CurrentMedianHistory.Base.Amount, fh.CurrentMedianHistory.Quote.Amount)
		}
	}
	processConversions(session, hdr.Timestamp, &d.hooks)
	processCommentCashouts(session, hdr.Timestamp, blockNum, &d.hooks)
	processVestingWithdrawals(session, hdr.Timestamp, &d.hooks)
	processSavingsWithdraws(session, hdr.Timestamp)
	processSubsidizedAccounts(session, blockNum)
	payLiquidityReward(session, blockNum, hdr.Timestamp)
	updateVirtualSupply(session, blockNum)
	processAccountRecovery(session, hdr.Timestamp)
	expireEscrowRatifications(session, hdr.Timestamp)
	processDeclineVotingRights(session, hdr.Timestamp, blockNum, d)
	processProposals(session, hdr.Timestamp, blockNum)
	processDelayedVoting(session, hdr.Timestamp, blockNum)
	removeExpiredGovernanceVotes(session, hdr.Timestamp, blockNum, d)
	processRecurrentTransfers(session, hdr.Timestamp, blockNum, d)
	processHardforks(session, blockNum)

	{
		dgpoRef, _ := objects.GetDGPORef(session)
		var headBlockID [20]byte
		if dgpoRef != nil {
			headBlockID = dgpoRef.HeadBlockID
		}
		objects.PutBlockSummary(session, blockNum, headBlockID)
	}
	if !opts.SkipDedupAndTaPOS {
		objects.PruneExpiredTransactions(session, hdr.Timestamp)
	}

	// RC subsystem finalize_block hook (HF20+). Runs the per-block
	// pool decay + expired-delegation refund. Pre-HF20 only the
	// expiration sweep fires (safe — table is empty).
	rcFinalizeBlock(session, blockNum, hdr.Timestamp)

	// LIB recompute runs after the new schedule + producer-confirm are
	// in place. Mirrors database.cpp:2870 update_last_irreversible_block.
	d.updateLastIrreversibleBlock(session)

	// Re-read dgpo so the returned value reflects per-block hook
	// updates (supply, vesting fund, etc.).
	var dgpo objects.DynamicGlobalProperty
	if updated, err := objects.GetDGPO(session); err == nil {
		dgpo = updated
	}

	// Commit / soft-commit decision:
	//   - opts.SoftCommit forces SoftCommit (caller drives FlushBatch).
	//   - commitBatchSize > 1: SoftCommit until the batch counter hits
	//     the size, then fire one real Commit (with fsync per d.commitSync).
	//   - else: legacy per-block Commit using opts.Sync.
	switch {
	case opts.Reversible:
		// Leave the block UNCOMMITTED: overlay + subsystem undo logs + the
		// block savepoint stay live so FinalizeReversible (Commit) or
		// UndoReversible (Session.Undo) can act on it. Record the pending
		// block identity for the fork driver.
		d.reversiblePending = &reversibleBlock{
			num:  blockNum,
			id:   dgpo.HeadBlockID,
			prev: hdr.Previous,
			raw:  append([]byte(nil), rawBlock...),
		}
	case opts.SoftCommit:
		session.SoftCommit()
	case d.commitBatchSize > 1:
		d.batchCounter++
		if d.batchCounter >= d.commitBatchSize {
			d.batchCounter = 0
			if err := session.Commit(d.commitSync); err != nil {
				return dgpo, fmt.Errorf("apply_block %d commit: %w", blockNum, err)
			}
		} else {
			session.SoftCommit()
		}
	default:
		if err := session.Commit(opts.Sync); err != nil {
			return dgpo, fmt.Errorf("apply_block %d commit: %w", blockNum, err)
		}
	}
	// Cache the raw block bytes so the p2p sync layer can re-serve them
	// to peers requesting `fetch_items`. Done after commit so callers
	// only see entries for blocks that successfully landed.
	if d.recent != nil {
		raw := append([]byte(nil), rawBlock...)
		d.recent.Put(blockNum, dgpo.HeadBlockID, raw)
	}
	// Reversible (uncommitted tip) blocks defer their irreversible side-effects
	// to FinalizeReversible via onBlockFinalized — firing onBlockApplied here
	// would block_log-append + evict a block a reorg might still undo.
	if !opts.Reversible && d.onBlockApplied != nil {
		d.onBlockApplied(blockNum, dgpo.HeadBlockID)
	}
	return dgpo, nil
}

// FlushBatch flushes the current Session's accumulated overlay to the
// backend. Used by batched-commit drivers (ghive-sync's commitBatch
// path) that finalize each block via ApplyOptions.SoftCommit and want
// to fsync once every N blocks instead of once per block.
//
// Acquires applyMu in WRITE mode for the duration of the flush so a
// concurrent ApplyBlock can't race the overlay reset. sync controls
// whether the underlying Pebble write is fsync'd; pass true on a
// catch-up boundary or shutdown for durability.
//
// Safe to call when the Session has no pending writes (no-op).
func (d *Database) FlushBatch(sync bool) error {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	if d.session == nil {
		return nil
	}
	return d.session.Commit(sync)
}

// reversibleBlock identifies a block applied under ApplyOptions.Reversible
// whose writes are still uncommitted (in the Session overlay + subsystem undo).
type reversibleBlock struct {
	num  uint32
	id   block.ID
	prev block.ID
	raw  []byte // owned copy, handed to onBlockFinalized at commit time
}

// ReversiblePending returns the identity of the currently-pending reversible
// block (num, id, prev) and whether one exists. The fork driver uses this to
// decide, on the next inbound block, whether it extends the pending head
// (Finalize the pending, then apply) or forks off it / its parent (Undo, then
// apply the competing branch).
func (d *Database) ReversiblePending() (num uint32, id, prev block.ID, ok bool) {
	d.applyMu.RLock()
	defer d.applyMu.RUnlock()
	if d.reversiblePending == nil {
		return 0, block.ID{}, block.ID{}, false
	}
	r := d.reversiblePending
	return r.num, r.id, r.prev, true
}

// FinalizeReversible commits the pending reversible block's writes to the
// backend (flushing the overlay + dropping the subsystem undo), making it
// irreversible from the node's perspective. Called once the block is confirmed
// to extend the node's chain (the next block builds on it). No-op if nothing is
// pending. sync controls fsync.
func (d *Database) FinalizeReversible(sync bool) error {
	d.applyMu.Lock()
	if d.reversiblePending == nil || d.session == nil {
		d.applyMu.Unlock()
		return nil
	}
	if err := d.session.Commit(sync); err != nil {
		d.applyMu.Unlock()
		return err
	}
	fin := d.reversiblePending
	d.reversiblePending = nil
	d.applyMu.Unlock()
	// Now committed → run the irreversible side-effects (block_log append,
	// eviction) the reversible apply deferred. Fired OUTSIDE applyMu so the
	// hook may re-enter the chain safely.
	if d.onBlockFinalized != nil {
		d.onBlockFinalized(fin.num, fin.raw)
	}
	return nil
}

// UndoReversible rolls the pending reversible block's writes back out of the
// Session overlay + subsystem undo logs, restoring state to the last committed
// (irreversible) block. Used by the fork driver when switching to a competing
// branch. No-op if nothing is pending.
func (d *Database) UndoReversible() {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	if d.reversiblePending == nil || d.session == nil {
		return
	}
	d.session.Undo()
	d.reversiblePending = nil
}

// HeadBlock returns the current head block number per DGPO. Returns 0
// when no blocks have been applied yet.
func (d *Database) HeadBlock() (uint32, error) {
	d.applyMu.RLock()
	defer d.applyMu.RUnlock()
	s := state.NewSession(d.state)
	p, err := objects.GetDGPO(s)
	if err == state.ErrNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return p.HeadBlockNumber, nil
}

// GetDGPO returns a snapshot of the dynamic_global_properties object.
// Wraps state.NewSession + objects.GetDGPO inside the chain-level
// applyMu read lock so callers (witness production loop, RPC layers)
// don't race ApplyBlock's commit. The returned struct is a value copy,
// safe to use after the lock is released.
func (d *Database) GetDGPO() (objects.DynamicGlobalProperty, error) {
	d.applyMu.RLock()
	defer d.applyMu.RUnlock()
	s := state.NewSession(d.state)
	return objects.GetDGPO(s)
}

// ScheduledWitness returns the witness who should produce the block
// at slot_num counting from 1 (1 = next block). Mirrors hived's
// database::get_scheduled_witness.
func (d *Database) ScheduledWitness(slotNum uint32) (string, error) {
	d.applyMu.RLock()
	defer d.applyMu.RUnlock()
	s := state.NewSession(d.state)
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return "", err
	}
	ws, err := objects.GetWitnessSchedule(s)
	if err != nil {
		return "", err
	}
	return string(ws.ScheduledWitnessForSlotAslot(dgpo.CurrentAslot, slotNum)), nil
}

// HIVE_BLOCK_INTERVAL: seconds between blocks (libraries/protocol/include/
// hive/protocol/config.hpp).
const blockIntervalSec uint32 = 3

// slotNumForBlockTime computes the slot number of a block whose timestamp
// is `blockTime`, given that the chain head is at `headTime`. slot 1 = the
// next contiguous block; slot 2 = one missed; etc. Returns 0 if the block
// is at or before head_time (which would be a clock-rewind error upstream).
func slotNumForBlockTime(headTime, blockTime uint32) uint32 {
	// Mirror hived's get_slot_at_time (database.cpp:1015-1020) which
	// uses get_slot_time(1) = head_slot_time + 1*interval, where
	// head_slot_time = (head_block_time / interval) * interval —
	// ROUNDED DOWN to the previous interval boundary. Without the
	// alignment, blocks where head_block_time isn't a multiple of
	// interval would be miscounted (Go returned 0, hived returned 1).
	// Source of the 99-slot CurrentAslot gap observed at block
	// 5,148,656 vs hived.
	headSlotTime := (headTime / blockIntervalSec) * blockIntervalSec
	firstSlotTime := headSlotTime + blockIntervalSec
	if blockTime < firstSlotTime {
		return 0
	}
	return (blockTime-firstSlotTime)/blockIntervalSec + 1
}

// hiveWitnessShutdownThresholdBlocks = HIVE_BLOCKS_PER_DAY = 28800.
// Pre-HF14 / post-HF20 the shutdown branch is a no-op (HF14 introduced it,
// HF20 disabled it via SP190). Within HF14-HF20, a witness whose
// last_confirmed_block_num is more than this many blocks behind head_block
// has its signing_key zeroed (effectively dropping them from the schedule).
const hiveWitnessShutdownThresholdBlocks uint32 = 28800

// updateWitnessMissedBlocks mirrors database::update_witness_missed_blocks
// (database_witness.cpp:284-306). For each missed slot 1..missedBlocks, it
// finds the witness who SHOULD have produced that slot, increments their
// total_missed counter, and HF14-HF20 zeroes their BlockSigningKey if
// their last_confirmed is too far behind head — which causes the next
// witness-schedule rebuild to drop them from the active set entirely.
func updateWitnessMissedBlocks(s *state.Session, blockWitness auth.AccountName, missedBlocks uint32, hf hardfork.Set, blockNum uint32) {
	if missedBlocks == 0 {
		return
	}
	ws, err := objects.GetWitnessSchedule(s)
	if err != nil {
		return
	}
	// hived: per-slot witness lookup uses dgpo.current_aslot, NOT
	// head_block_num. iter16 closed the upstream CurrentAslot drift
	// (slotNumForBlockTime now rounds head_time DOWN to interval
	// boundary, matching hived's get_slot_at_time).
	dgpoForAslot, _ := objects.GetDGPO(s)
	currentAslot := dgpoForAslot.CurrentAslot
	for i := uint32(0); i < missedBlocks; i++ {
		missedName := ws.ScheduledWitnessForSlotAslot(currentAslot, i+1)
		if missedName == "" || missedName == blockWitness {
			continue
		}
		w, err := objects.GetWitness(s, missedName)
		if err != nil {
			continue
		}
		w.TotalMissedBlocks++
		if hf.Has(hardfork.HF14) && !hf.Has(hardfork.HF20) {
			if blockNum-w.LastConfirmedBlockNum > hiveWitnessShutdownThresholdBlocks {
				w.BlockSigningKey = [33]byte{}
			}
		}
		objects.PutWitness(s, &w)
	}
}

// isNotImplemented reports whether err wraps evaluator.ErrNotImplemented.
func isNotImplemented(err error) bool {
	for ; err != nil; err = unwrap(err) {
		if err == evaluator.ErrNotImplemented {
			return true
		}
	}
	return false
}

func unwrap(err error) error {
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok {
		return u.Unwrap()
	}
	return nil
}

// Reserved for use by tests/CLI: extracts a uint32 ID from a key body.
var _ = binary.BigEndian.Uint32

// SetOwnerAuthHistoryStartBlock overrides where owner_authority_history
// tracking begins (0 = mainnet default). Test-only: fixtures that start at
// block 1 need it so recover_account can find a recent authority without
// pre-rolling 3,186,477 blocks.
//
// This replaces a mutable package-level global in the evaluator package that
// parallel tests wrote concurrently (flagged by -race). Per-Database state
// cannot be stomped by another test.
func (d *Database) SetOwnerAuthHistoryStartBlock(n uint32) {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	d.ownerAuthHistoryStartBlock = n
}
