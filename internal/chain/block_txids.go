package chain

import (
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/tx"
)

// BlockTransactionIDs returns the transaction ids carried by a serialized
// block, in order. Returns nil for an empty or unparseable block.
//
// Used to evict mempool entries once some other witness has already included
// them. Without that eviction the mempool only ever grows: every transaction we
// admit is mined by someone else within a few seconds, nothing removes it, and
// by the time our slot arrives the whole queue is duplicates that pre-flight
// rejects — so we produce an EMPTY block while the network is busy. Observed on
// mainnet 2026-08-10: depth climbing 598→1082 monotonically, and both blocks we
// produced (108891828, 108907971) empty beside neighbours carrying 13-41.
//
// It is also a slow-motion wedge: at MaxMempoolTx the queue stops accepting new
// transactions altogether, so the emptiness becomes permanent rather than
// merely likely.
func BlockTransactionIDs(rawBlock []byte) [][20]byte {
	_, hdrLen, err := block.UnmarshalSignedHeader(rawBlock)
	if err != nil {
		return nil
	}
	r := fcraw.NewReader(rawBlock[hdrLen:])
	txCount, err := r.ReadVarUint()
	if err != nil || txCount == 0 {
		return nil
	}
	out := make([][20]byte, 0, txCount)
	for i := uint32(0); i < txCount; i++ {
		txRes, rerr := tx.ReadSignedTransaction(r)
		if rerr != nil {
			return out // partial result: the prefix we did parse is still valid
		}
		unsigned := rawBlock[hdrLen+txRes.UnsignedStart : hdrLen+txRes.UnsignedEnd]
		out = append(out, tx.IDFromDigest(tx.DigestFromBytes(unsigned)))
	}
	return out
}
