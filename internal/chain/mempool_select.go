package chain

import (
	"fmt"
	"time"

	"hived-go/internal/chain/evaluator"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/network"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// maxSelectDuration caps how long transaction selection may run. The slot
// budget is 3s and selection holds the apply lock throughout; overrunning it
// costs the block entirely, which is strictly worse than shipping a partial
// one.
const maxSelectDuration = 750 * time.Millisecond

// minTxBytes is a floor on a serialized transaction, used to stop looking once
// no further candidate could possibly fit.
const minTxBytes = 64

// maxSelectCandidates caps how many candidates one selection may evaluate.
const maxSelectCandidates = 1500

// maxSelectEvaluated exposes the cap for tests.
func maxSelectEvaluated() int { return maxSelectCandidates }

// PendingTxOutcome says what happened to one candidate transaction.
type PendingTxOutcome int

const (
	PendingAccepted PendingTxOutcome = iota
	PendingBadSignature                 // authority/signature check failed
	PendingPreFlightFailed              // expiration window, dedup, or TaPOS
	PendingApplyFailed                  // an operation failed against head state
	PendingTooLarge                     // would exceed the block size budget
	PendingMalformed                    // undecodable / no operations
	PendingInsufficientRC               // payer cannot afford it (what hived refuses at admission)
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
	case PendingInsufficientRC:
		return "insufficient-rc"
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
	producer string,
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
		// The PRODUCER of the block being built — not dgpo.CurrentWitness, which
		// is the PREVIOUS block's producer. ApplyBlock uses hdr.Witness here, and
		// a trial apply that does not model the real apply can approve a
		// transaction that then fails, which invalidates the whole block. Only
		// pow.go reads this today (dead on mainnet), so the divergence is
		// currently harmless — but "currently harmless" is not a property worth
		// depending on in block assembly.
		Witness:       producer,
		// Virtual ops emitted during selection are discarded: this is a trial
		// run, and the real ones are produced when the block is actually
		// applied. Feeding them to the live buffer would duplicate every vop.
		EmitVOp: func(any) {},
	}

	// Bound the work. Selection runs under the apply lock inside a 3s slot, and
	// each candidate costs a signature recovery plus a trial apply (~100µs+).
	// With the mempool at its cap that is thousands of candidates — enough to
	// eat the slot we are trying to fill. Roughly half the queue is transactions
	// nobody will ever mine, so evaluating all of it is wasted work as well as
	// slow. Callers pass candidates NEWEST-FIRST, so the useful ones are seen
	// before the budget runs out.
	evaluated := 0
	deadline := time.Now().Add(maxSelectDuration)

	for i, raw := range candidates {
		// Stop when the block is full, when we have looked at enough, or when
		// we are out of time — a partial block beats a missed slot.
		if sel.Bytes >= maxBytes-minTxBytes || evaluated >= maxSelectCandidates {
			break
		}
		if evaluated&0x3f == 0 && time.Now().After(deadline) {
			break
		}
		if len(raw) == 0 {
			sel.Rejected[i] = PendingMalformed
			continue
		}
		if sel.Bytes+len(raw) > maxBytes {
			sel.Rejected[i] = PendingTooLarge
			continue
		}
		evaluated++
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
		// CHARGE AND ENFORCE RC, exactly as a producing hived node does.
		//
		// hived enforces RC while is_in_control() — i.e. at admission and block
		// production — and NOT when applying someone else's block (an RC-based
		// block rejection would fork the chain). Omitting it here does not make
		// our block invalid, but it makes us include transactions every other
		// witness deliberately refuses.
		//
		// This is why roughly half the transactions we admit are never mined by
		// anyone: they are RC-insufficient, hived declines them at admission,
		// and only we were willing to carry them. Enforcing here keeps us from
		// becoming the spam path onto the chain, and charging is also required
		// for correctness — without it, several transactions from the same payer
		// could each pass on the payer's full mana and collectively overdraw it.
		if hf.Has(hardfork.HF20) {
			rcErr := applyRCForTx(session, &res.SignedTx, blockNum, blockTime,
				int64(res.SignedEnd-res.UnsignedStart), true /* enforce */)
			if rcErr != nil {
				session.Restore(txSnap)
				sel.Rejected[i] = PendingInsufficientRC
				continue
			}
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
	// NO RC check at admission, deliberately.
	//
	// hived enforces RC inside push_transaction, which runs when a node accepts
	// a gossiped transaction — so an RC-insufficient transaction is refused by
	// the FIRST node that sees it and never propagates. Anything reaching us has
	// already cleared RC somewhere upstream.
	//
	// Our RC cost model is approximate. Re-deciding a question hived has already
	// answered can therefore only lose valid transactions when our numbers
	// differ slightly from theirs — thinning our blocks for no protection.
	// Affordability is re-checked at BUILD time (SelectPendingTransactions),
	// where a transaction may genuinely have become unaffordable because the
	// payer's mana was consumed after admission, and where a wrong answer costs
	// one transaction rather than poisoning the queue.
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
