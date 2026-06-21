package chain

import (
	"fmt"

	"hived-go/internal/chain/evaluator"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
)

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

	// Phase 2: parallel speculation. Empty whitelist (this increment) → no tx
	// is parallelizable, so nothing is speculated and every tx falls to the
	// serial commit path below. Coverage grows op-by-op in later increments.
	// (Future: schedule txParallelizable(txs[i]) across `workers` goroutines,
	// each running applyTxSpeculative on session.ForkSpeculative().)
	_ = workers

	// Phase 3: serial commit in transaction order.
	for i := range txs {
		signed := &txs[i].SignedTx
		signedLen := txs[i].SignedEnd - txs[i].UnsignedStart
		if err := d.applyTxSerial(ctx, session, signed, blockNum, hdrTimestamp, signedLen, opts, stats); err != nil {
			return 0, fmt.Errorf("apply_block %d tx %d: %w", blockNum, i, err)
		}
	}
	return 0, nil
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
	return false
}
