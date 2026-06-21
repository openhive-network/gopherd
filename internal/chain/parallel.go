package chain

import (
	"fmt"
	"sync"
	"sync/atomic"

	"hived-go/internal/chain/evaluator"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Coverage counters for the parallel harness (diagnostics only; not consensus
// state). specAppliedTxs counts transactions applied from a validated
// speculative write-set; specReexecTxs counts those re-executed serially at
// commit (taint / read-set conflict / unsupported op mix).
var (
	specAppliedTxs atomic.Int64
	specReexecTxs  atomic.Int64
)

// ParallelStats returns (speculatively-applied, serially-re-executed) tx counts.
func ParallelStats() (applied, reexec int64) {
	return specAppliedTxs.Load(), specReexecTxs.Load()
}

// applyTxsParallel applies a block's transactions using the optimistic parallel
// harness (see PARALLEL_DESIGN.md). It REPLACES the serial tx loop in ApplyBlock
// when opts.ParallelApply > 0 in replay mode. It returns the number of
// transactions skipped (always 0 in replay: a failing op halts).
//
// Phases:
//  1. PARSE (sequential — tx framing is varint-chained, not random-access):
//     decode all txCount transactions from r into a slice. The op slabs are
//     monotonic within a block, so all parsed op pointers stay valid.
//  2. SPECULATE (parallel): for each transaction whose ops are ALL in the
//     speculative whitelist, fork a per-worker session and execute it against
//     the frozen committed state, recording a read-set and buffering writes.
//     (Whitelist is empty in this increment, so this phase is a no-op.)
//  3. COMMIT (serial, in tx order): for each transaction, if it was speculated
//     and its read-set still validates, apply its buffered write-set; otherwise
//     re-execute it serially against the real (now-mutating) state. Either way
//     the result equals serial order exactly.
func (d *Database) applyTxsParallel(
	ctx *evaluator.Context,
	session *state.Session,
	r *fcraw.Reader,
	txCount uint32,
	blockNum uint32,
	hdrTimestamp uint32,
	opts ApplyOptions,
	stats *Stats,
	workers int,
) (uint64, error) {
	// Phase 1: parse all transactions sequentially.
	txs := make([]tx.ReadResult, txCount)
	for i := uint32(0); i < txCount; i++ {
		txr, err := tx.ReadSignedTransactionSkipSigs(r)
		if err != nil {
			return 0, fmt.Errorf("apply_block %d tx %d parse: %w", blockNum, i, err)
		}
		txs[i] = txr
	}

	// Determine which transactions are eligible for speculation: every op tag
	// must be speculative-supported, and the block must be pre-HF20 (HF20+ RC
	// charging is a shared per-tx accumulator not yet handled commutatively).
	preHF20 := !hardfork.SetForBlock(blockNum).Has(hardfork.HF20)
	specSess := make([]*state.Session, len(txs))
	specOK := make([]bool, len(txs))

	// Phase 2: speculate eligible transactions in parallel against the frozen
	// committed state. Each worker forks a session (shared read view, private
	// write buffers) and runs the tx's ops; taint/error/panic ⇒ not speculated
	// (falls to serial re-exec at commit).
	if preHF20 && workers > 0 {
		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup
		for i := range txs {
			if !txParallelizable(&txs[i].SignedTx) {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				specSess[i], specOK[i] = d.speculateTx(ctx, session, &txs[i].SignedTx, blockNum)
			}(i)
		}
		wg.Wait()
	}

	// Phase 3: commit in transaction order. A speculated tx whose read-set
	// still validates and which buffered no K/V writes is applied from its
	// write buffer; everything else is re-executed serially against the real
	// (now-mutating) state. Either way the effect equals serial order.
	var valBuf []byte
	for i := range txs {
		signed := &txs[i].SignedTx
		if specOK[i] {
			ss := specSess[i]
			if ss.Spec.NumKVWrites() == 0 &&
				objects.SpecTypedReadsValid(ss, &valBuf) &&
				ss.Spec.SpecKVReadsValid(session.CommittedKVByPhysicalKey) {
				objects.SpecApplyTypedWrites(ss)
				if stats != nil {
					stats.OpsApplied += uint64(len(signed.Operations))
				}
				specAppliedTxs.Add(1)
				continue
			}
			specReexecTxs.Add(1)
		}
		signedLen := txs[i].SignedEnd - txs[i].UnsignedStart
		if err := d.applyTxSerial(ctx, session, signed, blockNum, hdrTimestamp, signedLen, opts, stats); err != nil {
			return 0, fmt.Errorf("apply_block %d tx %d: %w", blockNum, i, err)
		}
	}
	return 0, nil
}

// speculateTx executes one transaction optimistically against the frozen
// committed state on a forked session, returning the fork and whether the
// speculation is usable (no taint, no op error, no panic). The forked session's
// Spec holds the read-set and buffered writes for commit-time validation/apply.
func (d *Database) speculateTx(ctx *evaluator.Context, main *state.Session, signed *tx.SignedTransaction, blockNum uint32) (sess *state.Session, ok bool) {
	sess = main.ForkSpeculative()
	cctx := *ctx
	cctx.DB = sess
	// Virtual ops are not consensus state and the shared per-block buffer is
	// not concurrency-safe; speculated (parallel-applied) txs do not emit them.
	cctx.EmitVOp = func(any) {}
	defer func() {
		if r := recover(); r != nil {
			ok = false // a panic on garbage speculative state ⇒ re-exec serially
		}
	}()
	for _, o := range signed.Operations {
		if err := evaluator.ApplyOp(&cctx, o); err != nil {
			return sess, false
		}
		if sess.Spec.Tainted() {
			return sess, false
		}
	}
	return sess, !sess.Spec.Tainted()
}

// txParallelizable reports whether every op in the transaction is supported by
// the speculative execution layer.
func txParallelizable(signed *tx.SignedTransaction) bool {
	if len(signed.Operations) == 0 {
		return false
	}
	for _, o := range signed.Operations {
		if !opTagSpeculative(o.Tag) {
			return false
		}
	}
	return true
}

// applyTxSerial applies one transaction's operations to session in order,
// mirroring the replay-mode (SkipTransactionSignatures / SkipTxSavepoints /
// SkipDedupAndTaPOS) fast path of the serial tx loop in ApplyBlock: no
// per-tx savepoint, no dedup record, a failing op halts. Used by the harness
// for the serial-commit / re-execution path; ctx.DB must be `session`.
func (d *Database) applyTxSerial(
	ctx *evaluator.Context,
	session *state.Session,
	signed *tx.SignedTransaction,
	blockNum uint32,
	hdrTimestamp uint32,
	signedLen int,
	opts ApplyOptions,
	stats *Stats,
) error {
	resetRCDifferentialForTx(session, blockNum)
	for j, o := range signed.Operations {
		applyRCDifferentialForOp(session, o, blockNum)
		if err := evaluator.ApplyOp(ctx, o); err != nil {
			return fmt.Errorf("op %d (%s): %w", j, o.Tag.Name(), err)
		}
		if stats != nil {
			stats.OpsApplied++
		}
	}
	// HF20+: charge RC for the whole transaction.
	if hardfork.SetForBlock(blockNum).Has(hardfork.HF20) && len(signed.Operations) > 0 {
		if rcErr := applyRCForTx(session, signed, blockNum, hdrTimestamp, int64(signedLen), opts.EnforceRC); rcErr != nil {
			return rcErr
		}
	}
	return nil
}

// opTagSpeculative reports whether an op tag is supported by the speculative
// execution layer. A transaction is parallelizable iff every one of its ops is
// speculative-supported. Empty for now; grows as accessor coverage lands.
func opTagSpeculative(t op.Tag) bool {
	switch t {
	case op.TagTransfer:
		// Touches only Account balances (HIVE) or Account+DGPO (HBD/treasury,
		// which taints → serial). Both handled by the Account/DGPO spec layer.
		return true
	default:
		return false
	}
}
