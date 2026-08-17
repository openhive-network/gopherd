package chain

import (
	"fmt"
	"time"

	"hived-go/internal/chain/evaluator"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/network"
	"hived-go/internal/protocol/op"
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

// rcRejectLog, when set, receives every RC rejection during block selection.
// Diagnostic hook for the known RC over-strictness; nil disables.
var rcRejectLog func(ops []op.Op, err error)

// SetRCRejectLog installs the RC-rejection diagnostic hook.
func SetRCRejectLog(f func(ops []op.Op, err error)) { rcRejectLog = f }

// maxSelectEvaluated exposes the cap for tests.
func maxSelectEvaluated() int { return maxSelectCandidates }

// PendingTxOutcome says what happened to one candidate transaction.
type PendingTxOutcome int

const (
	PendingAccepted        PendingTxOutcome = iota
	PendingBadSignature                     // authority/signature check failed
	PendingPreFlightFailed                  // expiration window, dedup, or TaPOS
	PendingApplyFailed                      // an operation failed against head state
	PendingTooLarge                         // would exceed the block size budget
	PendingMalformed                        // undecodable / no operations
	PendingInsufficientRC                   // payer cannot afford it (what hived refuses at admission)
	PendingCustomOpLimit                    // account is at its per-block custom-op cap
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
	case PendingCustomOpLimit:
		return "custom-op-limit"
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

	// Per-account custom-operation budget for THIS block, the same rule
	// ApplyBlock enforces. Without it we pack in every custom_json we hold and
	// the block is INVALID — a witness may only include customOpBlockLimit per
	// account per block. That is very likely why 108912883 was rejected (579
	// transactions against the real block's 90) and why our mempool grew to
	// 2020 while hived's stayed near 117: hived refuses the excess, nobody can
	// include it, and only we kept holding it.
	customOps := make(map[auth.AccountName]uint8, 64)

	ctx := &evaluator.Context{
		OwnerAuthHistoryStartBlock: d.ownerAuthHistoryStartBlock,
		DB:                         session,
		Hardforks:                  hf,
		BlockNum:                   blockNum,
		BlockTime:                  blockTime,
		HeadBlockTime:              headBlockTime,
		// The PRODUCER of the block being built — not dgpo.CurrentWitness, which
		// is the PREVIOUS block's producer. ApplyBlock uses hdr.Witness here, and
		// a trial apply that does not model the real apply can approve a
		// transaction that then fails, which invalidates the whole block. Only
		// pow.go reads this today (dead on mainnet), so the divergence is
		// currently harmless — but "currently harmless" is not a property worth
		// depending on in block assembly.
		Witness: producer,
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
		// Custom-op budget. Checked BEFORE the trial apply so a transaction that
		// cannot legally go in this block costs nothing to reject, and so the
		// counter only advances for transactions we actually keep.
		if cerr := applyCustomOpBlockLimit(customOps, &res.SignedTx); cerr != nil {
			sel.Rejected[i] = PendingCustomOpLimit
			continue
		}

		// Trial apply. A failure rolls back only this transaction, so a bad
		// candidate cannot contaminate the ones selected after it.
		txSnap := session.Snapshot()
		if aerr := applyOpsTrial(ctx, &res.SignedTx, blockNum); aerr != nil {
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
				// Log the payer and the shortfall. Our RC cost accounting is
				// APPROXIMATE and measurably STRICTER than hived's: the block
				// comparison caught us refusing a transaction the real block
				// INCLUDED (WE-HAD-IT-AND-DROPPED, ~1 in 100), which means we
				// silently drop valid traffic. Without the account and the
				// numbers there is nothing to compare against hived's rc_api,
				// so capture them here rather than re-deriving them later.
				if rcRejectLog != nil {
					rcRejectLog(res.SignedTx.Operations, rcErr)
				}
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
func applyOpsTrial(ctx *evaluator.Context, signed *tx.SignedTransaction, blockNum uint32) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("panic during trial apply: %v", r)
		}
	}()
	for _, o := range signed.Operations {
		// Mirror ApplyBlock's per-op sequence exactly: the RC differential pass
		// runs BEFORE the evaluator, and applyRCForTx later reads what it
		// accumulated. Skipping it makes the trial compute a DIFFERENT resource
		// cost from the real apply, so a transaction could be accepted here and
		// charged differently in the block — or rejected here that hived would
		// take. A trial apply is only useful insofar as it is the same apply.
		applyRCDifferentialForOp(ctx.DB, o, blockNum)
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
	// Measure how long admission WAITS for the read lock.
	//
	// Hypothesis under test: our blocks are short because transactions reach
	// the mempool late, and the lateness is uniform across the 3s slot rather
	// than a constant network offset — the signature of contention, not of a
	// gossip hop. Every transaction arriving while ApplyBlock holds the WRITE
	// lock queues here. Doubling peers changed nothing, which fits.
	start := time.Now()
	d.applyMu.RLock()
	d.recordAdmitLockWait(time.Since(start))
	defer d.applyMu.RUnlock()
	return d.validateIncomingTxLocked(raw)
}

// recordAdmitLockWait accumulates the admission lock-wait distribution.
func (d *Database) recordAdmitLockWait(w time.Duration) {
	ms := w.Milliseconds()
	d.admitWaitMu.Lock()
	d.admitWaitN++
	d.admitWaitTotalMs += ms
	if ms > d.admitWaitMaxMs {
		d.admitWaitMaxMs = ms
	}
	if ms >= 100 {
		d.admitWaitOver100++
	}
	d.admitWaitMu.Unlock()
}

// AdmitLockWaitStats reports (samples, meanMs, maxMs, countOver100ms).
func (d *Database) AdmitLockWaitStats() (int64, int64, int64, int64) {
	d.admitWaitMu.Lock()
	defer d.admitWaitMu.Unlock()
	if d.admitWaitN == 0 {
		return 0, 0, 0, 0
	}
	return d.admitWaitN, d.admitWaitTotalMs / d.admitWaitN, d.admitWaitMaxMs, d.admitWaitOver100
}

// validateIncomingTxLocked is ValidateIncomingTx's body, with the caller owning
// applyMu. Split out so the pre-apply admission audit — which runs inside
// ApplyBlock under the WRITE lock — can reuse the exact same check rather than
// a parallel copy that could drift from it.
func (d *Database) validateIncomingTxLocked(raw []byte) error {
	res, err := readSignedTxBytes(raw)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if len(res.SignedTx.Operations) == 0 {
		return fmt.Errorf("transaction has no operations")
	}

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
	// witness_block_approve MAY NEVER APPEAR IN A BLOCK. hived's evaluator is a
	// hard assert, not a validation rule (hive_evaluator.cpp:615):
	//
	//   HIVE_CHAIN_UNREACHABLE_CODE_ASSERT( false &&
	//     "This operation may not be included in a block" );
	//
	// It is a FAST-CONFIRMATION message, handled by
	// database::process_fast_confirm_transaction — an entirely separate path
	// from push_transaction. That is why it is ~92% of gossiped traffic and
	// appears in exactly ZERO real blocks.
	//
	// So it must never enter the block-building mempool. Keeping it out is what
	// made our blocks match; letting it in is what put 579 of them in block
	// 108912883 and got that block rejected.
	//
	// I briefly removed this after implementing the witness-signature authority
	// category, reasoning that an authority we can VERIFY beats one we decline.
	// That was the wrong lesson: these transactions are correctly signed — the
	// verification passes — and admitting them immediately regressed block
	// assembly from only-ours=0 to ours=379 vs theirs=32. Authority was never
	// the reason to exclude them; the reason is that no block may carry one.
	//
	// (The AuthorityWitness category is still implemented and correct. It is
	// what a fast-confirm handler would need, if we implement one.)
	for i := range res.SignedTx.Operations {
		if OpExcludedFromBlocks(res.SignedTx.Operations[i].Tag) {
			return fmt.Errorf("op %d: %s is a fast-confirm message and may never be included in a block (hive_evaluator.cpp:615)",
				i, res.SignedTx.Operations[i].Tag.Name())
		}
	}

	// Per-operation structural validation, which hived runs on every
	// transaction it accepts (database.cpp:2627, full_transaction->validate()).
	// We had the validators and never called them on this path.
	//
	// That is the likeliest reason our mempool filled with transactions nobody
	// mines (~20/block accumulating) and why block 108912883 was rejected: we
	// admitted structurally invalid transactions the rest of the network had
	// already refused, then put 579 of them in a block.
	//
	// Only ops implementing Validator are checked — 36 of 51 today, INCLUDING
	// the highest-volume real operations (vote, comment, comment_options,
	// transfer). That makes this the single biggest recall risk in the filter:
	// a validator even slightly stricter than hived's refuses genuine traffic
	// while every other metric stays green — the mempool looks clean, admitted
	// transactions all get mined, and our blocks are quietly short. The
	// admission audit (chain.AuditBlockAdmission) is what measures it. So this
	// filters what it can prove invalid and passes the rest. Deliberately
	// applied at ADMISSION only, never when applying someone else's block: if
	// our validators were ever stricter than hived's, rejecting a network block
	// would fork us off the chain, whereas rejecting a transaction only costs
	// that transaction.
	for i := range res.SignedTx.Operations {
		if v, ok := res.SignedTx.Operations[i].Operation().(op.Validator); ok {
			if verr := v.Validate(); verr != nil {
				return fmt.Errorf("op %d invalid: %w", i, verr)
			}
		}
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
