package chain

import (
	"runtime"
	"sync"
	"sync/atomic"

	"hived-go/internal/crypto"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/network"
	"hived-go/internal/protocol/tx"
)

// RecoveredBlockSigs is the result of a parallel pre-recovery pass over
// every transaction signature in a block. Slot i of TxKeys corresponds
// to transaction i in apply order; len(TxKeys) == number of txs in the
// block. Each inner slice has one PublicKey per tx.Signatures entry.
//
// Slots may be nil when recovery failed for that tx (non-canonical sig,
// recovery error). ApplyBlock falls back to the serial path in those
// cases so the canonical error message + per-tx Restore semantics are
// preserved.
type RecoveredBlockSigs struct {
	TxKeys [][]crypto.PublicKey
}

// recoverWorkers is the size of the within-block fan-out pool used by
// RecoverBlockTxSignerKeys. NumCPU is a reasonable default; the per-tx
// secp256k1 recovery is pure CPU and scales linearly with cores. We
// cap at 8 because the per-block fan-out cost (channel sends, slice
// alloc) starts to outweigh the recovery work past that.
var recoverWorkers = func() int {
	n := runtime.NumCPU()
	if n < 1 {
		return 1
	}
	if n > 8 {
		return 8
	}
	return n
}()

// minTxsForParallel is the threshold at which RecoverBlockTxSignerKeys
// switches from a serial recovery loop to the goroutine-fan-out pool.
// Below this the channel + goroutine overhead beats the wall-clock win
// from parallelism (each recovery is ~100µs, fan-out costs ~5µs).
const minTxsForParallel = 4

// RecoverBlockTxSignerKeys parses every transaction in rawBlock and
// returns the recovered signer pubkey set per tx. Pure CPU work — does
// no state access — so it is safe to call from the per-peer receive
// goroutine BEFORE the block is queued for the single applyLoop. The
// applyLoop later threads the result back into ApplyBlock via
// ApplyOptions.PreRecoveredTxKeys, which lets VerifyAuthority skip its
// own RecoverPublicKey calls (the dominant CPU cost in live-sync).
//
// For blocks with len(txs) >= minTxsForParallel the recovery is fanned
// out across recoverWorkers goroutines. Below the threshold we run
// serially so the channel+goroutine overhead stays out of the way.
//
// Returns a zero-value RecoveredBlockSigs (TxKeys == nil) when the
// block can't even be parsed. Per-tx failures are encoded by leaving
// TxKeys[i] == nil; ApplyBlock will fall through to its own VerifyAuthority
// for those, reproducing the canonical failure message.
func RecoverBlockTxSignerKeys(rawBlock []byte, blockNum uint32) RecoveredBlockSigs {
	hdr, hdrLen, err := block.UnmarshalSignedHeader(rawBlock)
	if err != nil {
		return RecoveredBlockSigs{}
	}
	_ = hdr

	r := fcraw.NewReader(rawBlock[hdrLen:])
	txCount, err := r.ReadVarUint()
	if err != nil {
		return RecoveredBlockSigs{}
	}
	if txCount == 0 {
		return RecoveredBlockSigs{TxKeys: nil}
	}

	chainID := network.OldChainID
	hf := hardfork.SetForBlock(blockNum)
	if hf.Has(hardfork.HF24) {
		chainID = network.ChainID
	}

	// First pass — parse every tx, capture the unsigned-byte slice and
	// the signatures. Parsing is sequential (the wire format is
	// length-prefixed and not splittable without a first walk).
	type txDesc struct {
		unsignedBytes []byte
		sigs          [][tx.SignatureSize]byte
	}
	descs := make([]txDesc, 0, txCount)
	for i := uint32(0); i < txCount; i++ {
		txRes, err := tx.ReadSignedTransaction(r)
		if err != nil {
			// A parse error invalidates the rest of the block — return
			// the partial result so ApplyBlock at least gets the valid
			// prefix; it'll re-parse anyway and emit the canonical error
			// from its own loop.
			return RecoveredBlockSigs{TxKeys: nil}
		}
		descs = append(descs, txDesc{
			unsignedBytes: rawBlock[hdrLen+txRes.UnsignedStart : hdrLen+txRes.UnsignedEnd],
			sigs:          txRes.SignedTx.Signatures,
		})
	}

	out := RecoveredBlockSigs{TxKeys: make([][]crypto.PublicKey, len(descs))}

	// Serial fast path for small blocks.
	if len(descs) < minTxsForParallel || recoverWorkers <= 1 {
		for i, d := range descs {
			out.TxKeys[i] = recoverOneTxKeys(chainID, d.unsignedBytes, d.sigs)
		}
		return out
	}

	// Parallel path: shard tx indices across recoverWorkers goroutines
	// via a shared atomic counter (no per-job channel send — keeps the
	// per-tx overhead at one atomic.Add). Each worker pulls indices
	// until exhausted and writes its slot in `out.TxKeys` (no overlap,
	// so no synchronisation needed there).
	var nextIdx int32
	var wg sync.WaitGroup
	wg.Add(recoverWorkers)
	for w := 0; w < recoverWorkers; w++ {
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt32(&nextIdx, 1)) - 1
				if i >= len(descs) {
					return
				}
				out.TxKeys[i] = recoverOneTxKeys(chainID, descs[i].unsignedBytes, descs[i].sigs)
			}
		}()
	}
	wg.Wait()
	return out
}

// recoverOneTxKeys is the per-tx recovery body. Returns nil on any
// canonicality / recovery failure so the caller (ApplyBlock) can fall
// back to its own VerifyAuthority loop for that single tx and surface
// the canonical error with its formatting + Restore semantics.
func recoverOneTxKeys(chainID network.Hash32, unsignedBytes []byte, sigs [][tx.SignatureSize]byte) []crypto.PublicKey {
	if len(sigs) == 0 {
		// Zero-sig txs are legal (pow_operation, etc.). Return a
		// non-nil empty slice so the apply path treats this as
		// "successfully recovered, just nothing to recover" instead of
		// re-running the serial recovery for a tx with no work to do.
		return []crypto.PublicKey{}
	}
	digest := crypto.SHA256Of2(chainID[:], unsignedBytes)
	keys := make([]crypto.PublicKey, 0, len(sigs))
	for _, sig := range sigs {
		var compact crypto.CompactSignature
		copy(compact[:], sig[:])
		if !compact.IsCanonical() {
			return nil
		}
		k, err := crypto.RecoverPublicKey(digest, compact)
		if err != nil {
			return nil
		}
		keys = append(keys, k)
	}
	return keys
}
