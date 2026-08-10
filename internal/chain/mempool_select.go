package chain

import (
	"fmt"

	"hived-go/internal/chain/evaluator"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/network"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// PendingTxOutcome says what happened to one candidate transaction.
type PendingTxOutcome int

const (
	PendingAccepted PendingTxOutcome = iota
	PendingBadSignature                 // authority/signature check failed
	PendingPreFlightFailed              // expiration window, dedup, or TaPOS
	PendingApplyFailed                  // an operation failed against head state
	PendingTooLarge                     // would exceed the block size budget
	PendingMalformed                    // undecodable / no operations
)

func (o PendingTxOutcome) String() string {
	switch o {
	case PendingAccepted:
		return "accepted"
	case PendingBadSignature:
		return "bad-signature"
	case PendingPreFlightFailed:
		return "preflight-failed"
	case PendingApplyFailed:
		return "apply-failed"
	case PendingTooLarge:
		return "too-large"
	default:
		return "malformed"
	}
}

// PendingSelection is the result of choosing transactions for a block we are
// about to produce.
type PendingSelection struct {
	// Accepted holds the raw signed_transaction bytes to put in the block, in
	// order, VERBATIM. They must not be re-encoded: a transaction's signatures
	// cover its own serialized bytes, so repacking invalidates them.
	Accepted [][]byte
	// Digests are the per-transaction merkle digests for Accepted, in the same
	// order.
	Digests []tx.Digest
	// Rejected maps the index of each candidate that was NOT accepted to why.
	// The caller uses this to evict permanently-dead transactions from its
	// mempool instead of retrying them every slot.
	Rejected map[int]PendingTxOutcome
	// Bytes is the total serialized size of Accepted.
	Bytes int
}

// SelectPendingTransactions chooses which mempool transactions go into a block
// we are about to produce, by ACTUALLY APPLYING each one against head state and
// keeping only those that succeed.
//
// Why apply rather than merely validate: a block containing a transaction that
// fails to apply is an invalid block. The network rejects it, we miss the slot,
// and (before fork-choice handled produced blocks) the node could wedge on it.
// Signature and pre-flight checks alone do not catch a transaction that is
// well-formed and authorised but cannot execute — insufficient balance, RC
// exhaustion, a conflicting earlier transaction in the same block. hived has
// the same property: it keeps _pending_tx speculatively applied and drops
// whatever no longer executes when it builds a block.
//
// Committed state is left EXACTLY as found. Every mutation happens between a
// savepoint and its restore, the same mechanism ApplyBlock uses per transaction.
// The whole selection runs under the apply lock so no block can land midway
// through and invalidate what we measured.
//
// maxBytes bounds the total serialized transaction bytes (the caller derives it
// from the witness's maximum_block_size, less header and count overhead).
// Candidates are considered in order; one that would overflow the budget is
// skipped (reported PendingTooLarge) and later, smaller ones still get a look,
// since a single fat transaction should not starve the rest of the block.
func (d *Database) SelectPendingTransactions(
	candidates [][]byte,
	blockNum uint32,
	blockTime uint32,
	maxBytes int,
) (PendingSelection, error) {
	sel := PendingSelection{Rejected: make(map[int]PendingTxOutcome)}
	if len(candidates) == 0 {
		return sel, nil
	}

	// Write lock: we mutate the session (and restore it). A concurrent
	// ApplyBlock landing halfway through would make every decision below stale.
	d.applyMu.Lock()
	defer d.applyMu.Unlock()

	session := d.session

	dgpo, err := objects.GetDGPO(session)
	if err != nil {
		return sel, fmt.Errorf("select_pending: dgpo: %w", err)
	}
	// headBlockTime is the PREVIOUS block's timestamp — what hived's
	// push_transaction sees. Using the new block's timestamp instead would
	// shrink the expiration window by one interval and reject the trailing edge
	// of otherwise-valid transactions.
	headBlockTime := dgpo.Time

	hf := hardfork.SetForBlock(blockNum)
	chainID := network.ChainID
	// Same rule as ApplyBlock: the chain-ID switch is gated on the BLOCK
	// TIMESTAMP against HF24's actual time, not on the block-number predicate.
	const hf24ActualTime uint32 = 1602703884
	if blockTime <= hf24ActualTime {
		chainID = network.OldChainID
	}

	// Outer savepoint: everything below is rolled back before we return, so a
	// produced block is selected against head state without ever changing it.
	blockSnap := session.Snapshot()
	defer session.Restore(blockSnap)

	ctx := &evaluator.Context{
		DB:            session,
		Hardforks:     hf,
		BlockNum:      blockNum,
		BlockTime:     blockTime,
		HeadBlockTime: headBlockTime,
		Witness:       dgpo.CurrentWitness,
		// Virtual ops emitted during selection are discarded: this is a trial
		// run, and the real ones are produced when the block is actually
		// applied. Feeding them to the live buffer would duplicate every vop.
		EmitVOp: func(any) {},
	}

	for i, raw := range candidates {
		if len(raw) == 0 {
			sel.Rejected[i] = PendingMalformed
			continue
		}
		if sel.Bytes+len(raw) > maxBytes {
			sel.Rejected[i] = PendingTooLarge
			continue
		}
		res, derr := readSignedTxBytes(raw)
		if derr != nil || len(res.SignedTx.Operations) == 0 {
			sel.Rejected[i] = PendingMalformed
			continue
		}
		unsigned := raw[res.UnsignedStart:res.UnsignedEnd]
		txID := tx.IDFromDigest(tx.DigestFromBytes(unsigned))

		if verr := VerifyAuthority(session, &res.SignedTx, unsigned, chainID, false); verr != nil {
			sel.Rejected[i] = PendingBadSignature
			continue
		}
		if perr := validateTxPreFlight(session, &res.SignedTx, blockNum, headBlockTime, txID, true); perr != nil {
			sel.Rejected[i] = PendingPreFlightFailed
			continue
		}

		// Trial apply. A failure rolls back only this transaction, so a bad
		// candidate cannot contaminate the ones selected after it.
		txSnap := session.Snapshot()
		if aerr := applyOpsTrial(ctx, &res.SignedTx); aerr != nil {
			session.Restore(txSnap)
			sel.Rejected[i] = PendingApplyFailed
			continue
		}
		session.DiscardSavepoint(txSnap)

		sel.Accepted = append(sel.Accepted, raw)
		sel.Digests = append(sel.Digests, tx.MerkleDigestFromBytes(raw))
		sel.Bytes += len(raw)
	}
	return sel, nil
}

// applyOpsTrial runs a transaction's operations, converting a panic into an
// error. Selection must never take the node down: mempool transactions are
// attacker-controlled input, and an operation that panics on hostile state
// would otherwise kill a producing witness.
func applyOpsTrial(ctx *evaluator.Context, signed *tx.SignedTransaction) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("panic during trial apply: %v", r)
		}
	}()
	for _, o := range signed.Operations {
		if err := evaluator.ApplyOp(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

// ValidateIncomingTx is the cheap admission check for a transaction arriving
// from a peer: decode, authority/signature, and the expiration/dedup/TaPOS
// pre-flight against current head state. It does NOT apply the transaction —
// that happens at block-build time in SelectPendingTransactions, because state
// moves between admission and production anyway.
//
// Runs under a read lock, so it can be called from peer receive goroutines
// without serialising against block application.
func (d *Database) ValidateIncomingTx(raw []byte) error {
	res, err := readSignedTxBytes(raw)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if len(res.SignedTx.Operations) == 0 {
		return fmt.Errorf("transaction has no operations")
	}

	d.applyMu.RLock()
	defer d.applyMu.RUnlock()

	session := state.NewSession(d.state)
	dgpo, err := objects.GetDGPO(session)
	if err != nil {
		return fmt.Errorf("dgpo: %w", err)
	}
	blockNum := dgpo.HeadBlockNumber + 1
	chainID := network.ChainID
	const hf24ActualTime uint32 = 1602703884
	if dgpo.Time <= hf24ActualTime {
		chainID = network.OldChainID
	}
	unsigned := raw[res.UnsignedStart:res.UnsignedEnd]
	txID := tx.IDFromDigest(tx.DigestFromBytes(unsigned))
	if err := VerifyAuthority(session, &res.SignedTx, unsigned, chainID, false); err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	if err := validateTxPreFlight(session, &res.SignedTx, blockNum, dgpo.Time, txID, true); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	return nil
}

// readSignedTxBytes decodes one complete signed_transaction from raw, rejecting
// trailing bytes. Trailing bytes matter: the transaction ID is taken over the
// unsigned range, so two different byte strings could otherwise share an ID and
// slip past dedup.
func readSignedTxBytes(raw []byte) (tx.ReadResult, error) {
	r := fcraw.NewReader(raw)
	res, err := tx.ReadSignedTransaction(r)
	if err != nil {
		return res, err
	}
	if !r.AtEnd() {
		return res, fmt.Errorf("%d trailing bytes after signed_transaction", r.Remaining())
	}
	return res, nil
}
