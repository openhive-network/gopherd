// Package tx implements hive::protocol::transaction and signed_transaction
// with FC binary encode/decode, plus the digest, transaction-id, and
// merkle-digest derivations used by consensus.
//
// Wire format (FC_REFLECT order):
//
//	transaction:
//	  ref_block_num     uint16 LE
//	  ref_block_prefix  uint32 LE
//	  expiration        uint32 LE   (time_point_sec)
//	  operations        vector<operation>
//	  extensions        flat_set<future_extensions>   (typically empty)
//
//	signed_transaction extends transaction with:
//	  signatures        vector<signature_type>        (each 65 raw bytes)
//
// digest()         = sha256(serialized transaction)            — header+ops+ext
// transaction.id() = first 20 bytes of digest()                — public tx_id
// merkle_digest()  = sha256(serialized signed_transaction)     — header+ops+ext+sigs
//
// All of the above use serialization_mode_controller::legacy pack mode.
// For pre-HF26 block_logs the on-disk bytes are already in that mode, so we
// can hash byte ranges directly out of the source buffer instead of
// round-tripping. Post-HF26 may require re-serialization in legacy mode.
package tx

import (
	"encoding/binary"
	"fmt"

	"hived-go/internal/crypto"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/op"
)

// SignatureSize is the on-wire size of one signature_type
// (fc::ecc::compact_signature = fc::array<unsigned char, 65>).
const SignatureSize = 65

// Transaction is hive::protocol::transaction.
type Transaction struct {
	RefBlockNum    uint16
	RefBlockPrefix uint32
	Expiration     uint32 // time_point_sec
	Operations     []op.Operation
	Extensions     []byte
}

// SignedTransaction extends Transaction with signatures.
type SignedTransaction struct {
	Transaction
	Signatures [][SignatureSize]byte
}

// Digest is the 32-byte sha256 of the serialized transaction (no signatures).
type Digest [32]byte

// ID is the 20-byte truncated transaction_id_type — first 20 bytes of Digest.
type ID [20]byte

// AppendTransactionBody writes the transaction's body (header + ops + ext)
// to dst. Operations are written via op.AppendOperation.
func AppendTransactionBody(dst []byte, t *Transaction) []byte {
	dst = binary.LittleEndian.AppendUint16(dst, t.RefBlockNum)
	dst = binary.LittleEndian.AppendUint32(dst, t.RefBlockPrefix)
	dst = binary.LittleEndian.AppendUint32(dst, t.Expiration)
	dst = fcraw.AppendVarUint(dst, uint32(len(t.Operations)))
	for _, o := range t.Operations {
		dst = op.AppendOperation(dst, o)
	}
	if len(t.Extensions) > 0 {
		return append(dst, t.Extensions...)
	}
	return append(dst, 0) // empty flat_set<future_extensions>
}

// AppendSignedTransaction writes the full signed_transaction body.
func AppendSignedTransaction(dst []byte, st *SignedTransaction) []byte {
	dst = AppendTransactionBody(dst, &st.Transaction)
	dst = fcraw.AppendVarUint(dst, uint32(len(st.Signatures)))
	for _, s := range st.Signatures {
		dst = append(dst, s[:]...)
	}
	return dst
}

// ReadResult is returned by ReadSignedTransaction. It carries both the
// decoded form and the byte-range positions of the unsigned and signed
// portions inside the source buffer, which lets callers compute digest and
// merkle_digest by hashing source bytes directly (avoiding a re-serialize).
type ReadResult struct {
	SignedTx SignedTransaction
	// Unsigned is the byte range [UnsignedStart, UnsignedEnd) — header + ops
	// + extensions, NOT including signatures.
	UnsignedStart, UnsignedEnd int
	// Signed is [UnsignedStart, SignedEnd) — entire signed_transaction.
	SignedEnd int
}

// ReadSignedTransaction parses a signed_transaction starting at r's current
// position, returning the structured form and the byte ranges hashed by the
// digest / merkle-digest functions.
func ReadSignedTransaction(r *fcraw.Reader) (ReadResult, error) {
	return readSignedTransactionImpl(r, false)
}

// ReadSignedTransactionSkipSigs is the trusted-replay variant: parses the
// body + advances past the signatures WITHOUT allocating
// res.SignedTx.Signatures. Saves ~1.26 GB cumulative allocs across an 8M-
// block replay (every tx allocs a [][65]byte slice; replay never reads it).
func ReadSignedTransactionSkipSigs(r *fcraw.Reader) (ReadResult, error) {
	return readSignedTransactionImpl(r, true)
}

func readSignedTransactionImpl(r *fcraw.Reader, skipSigs bool) (ReadResult, error) {
	var res ReadResult
	res.UnsignedStart = r.Pos()

	tx, err := readTransactionBody(r)
	if err != nil {
		return res, err
	}
	res.UnsignedEnd = r.Pos()
	res.SignedTx.Transaction = tx

	// Signatures.
	count, err := r.ReadVarUint()
	if err != nil {
		return res, fmt.Errorf("tx.signatures count: %w", err)
	}
	if skipSigs {
		if err := r.Skip(int(count) * SignatureSize); err != nil {
			return res, fmt.Errorf("tx.signatures(skip): %w", err)
		}
	} else {
		res.SignedTx.Signatures = make([][SignatureSize]byte, count)
		for i := range res.SignedTx.Signatures {
			if err := r.ReadInto(res.SignedTx.Signatures[i][:]); err != nil {
				return res, fmt.Errorf("tx.signatures[%d]: %w", i, err)
			}
		}
	}
	res.SignedEnd = r.Pos()
	return res, nil
}

func readTransactionBody(r *fcraw.Reader) (Transaction, error) {
	var t Transaction
	var err error
	if t.RefBlockNum, err = r.ReadUint16(); err != nil {
		return t, fmt.Errorf("tx.ref_block_num: %w", err)
	}
	if t.RefBlockPrefix, err = r.ReadUint32(); err != nil {
		return t, fmt.Errorf("tx.ref_block_prefix: %w", err)
	}
	if t.Expiration, err = r.ReadUint32(); err != nil {
		return t, fmt.Errorf("tx.expiration: %w", err)
	}

	count, err := r.ReadVarUint()
	if err != nil {
		return t, fmt.Errorf("tx.operations count: %w", err)
	}
	t.Operations = make([]op.Operation, 0, count)
	for i := uint32(0); i < count; i++ {
		o, err := op.ReadOperation(r)
		if err != nil {
			return t, fmt.Errorf("tx.operations[%d]: %w", i, err)
		}
		t.Operations = append(t.Operations, o)
	}

	// Extensions: flat_set<future_extensions> — only variant is void_t.
	extStart := r.Pos()
	c, err := r.ReadVarUint()
	if err != nil {
		return t, fmt.Errorf("tx.extensions count: %w", err)
	}
	if c != 0 {
		// future_extensions has only one variant (void_t, tag 0, 0 bytes).
		// Decode each entry's tag and assert it's 0.
		for i := uint32(0); i < c; i++ {
			tag, err := r.ReadVarUint()
			if err != nil {
				return t, fmt.Errorf("tx.extensions[%d] tag: %w", i, err)
			}
			if tag != 0 {
				return t, fmt.Errorf("tx.extensions[%d]: unsupported variant tag %d", i, tag)
			}
		}
	}
	t.Extensions = append([]byte(nil), r.SourceRange(extStart, r.Pos())...)
	return t, nil
}

// DigestFromBytes returns sha256 of the unsigned-transaction byte range.
// `unsigned` must be exactly the bytes from UnsignedStart to UnsignedEnd of
// a ReadResult — i.e. the FC-binary form of `transaction` (no signatures).
func DigestFromBytes(unsigned []byte) Digest {
	return Digest(crypto.SHA256(unsigned))
}

// IDFromDigest returns the public 20-byte transaction_id (first 20 bytes of
// the sha256 digest).
func IDFromDigest(d Digest) ID {
	var id ID
	copy(id[:], d[:20])
	return id
}

// MerkleDigestFromBytes returns sha256 of the **full signed_transaction**
// byte range — the input to the block's merkle root.
func MerkleDigestFromBytes(signed []byte) Digest {
	return Digest(crypto.SHA256(signed))
}

// MerkleRoot computes the block-header transaction_merkle_root from a list
// of per-tx merkle digests. Algorithm (libraries/chain/full_block.cpp:620):
//
//   - Empty transaction list → all-zero 20-byte checksum.
//   - Otherwise: pairwise sha256(left || right) reduce; an odd-count layer
//     carries the last element through unchanged.
//   - Final reduction: ripemd160(final_sha256).
func MerkleRoot(digests []Digest) [20]byte {
	if len(digests) == 0 {
		return [20]byte{}
	}
	level := make([]Digest, len(digests))
	copy(level, digests)
	var pair [64]byte
	for len(level) > 1 {
		halfPaired := len(level) - (len(level) & 1)
		k := 0
		for i := 0; i < halfPaired; i += 2 {
			copy(pair[:32], level[i][:])
			copy(pair[32:], level[i+1][:])
			level[k] = Digest(crypto.SHA256(pair[:]))
			k++
		}
		if len(level)&1 == 1 {
			level[k] = level[len(level)-1]
			k++
		}
		level = level[:k]
	}
	return crypto.RIPEMD160(level[0][:])
}
