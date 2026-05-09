package chain

import (
	"fmt"

	"hived-go/internal/crypto"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// validateBlock runs the block-level invariants that hived's
// database::_apply_block enforces before any transaction is applied.
// Mirrors database.cpp:2335-2403.
//
// Skipped when opts.SkipBlockSignature is true (the canonical replay
// path). Live sync sets it false.
//
//	1. previous_id must equal dgpo.head_block_id (chain continuity).
//	2. block size ≤ wso.median_maximum_block_size.
//	3. transaction merkle root matches the header's recorded root.
//	4. HF5+: producing witness's running_version ≥ wso.majority_version.
//
// Header extension processing and the witness-scheduling assert live
// alongside in database.go ApplyBlock.
func validateBlock(s *state.Session, hdr *block.SignedHeader, rawBlock []byte, hdrLen int, blockNum uint32, hf hardfork.Set) error {
	if blockNum == 0 {
		return fmt.Errorf("validate_block: invalid block_num 0")
	}

	// 1) Chain continuity.
	if blockNum > 1 {
		if dgpo, err := objects.GetDGPO(s); err == nil {
			var zero [20]byte
			if dgpo.HeadBlockID != zero && hdr.Previous != dgpo.HeadBlockID {
				return fmt.Errorf("validate_block %d: previous_id mismatch: header=%x dgpo=%x",
					blockNum, hdr.Previous[:8], dgpo.HeadBlockID[:8])
			}
		}
	}

	// 2) Block size.
	ws, wsErr := objects.GetWitnessSchedule(s)
	if wsErr == nil && ws.MedianMaximumBlockSize > 0 {
		if uint32(len(rawBlock)) > ws.MedianMaximumBlockSize {
			return fmt.Errorf("validate_block %d: block size %d > max %d",
				blockNum, len(rawBlock), ws.MedianMaximumBlockSize)
		}
	}

	// 3) Merkle root.
	if err := validateMerkleRoot(hdr, rawBlock, hdrLen, blockNum); err != nil {
		return err
	}

	// 4) HF5+: running_version assert.
	if hf.Has(hardfork.HF5) && wsErr == nil && ws.MajorityVersion > 0 {
		if w, err := objects.GetWitness(s, hdr.Witness); err == nil {
			if w.RunningVersion < ws.MajorityVersion {
				return fmt.Errorf("validate_block %d: witness %q running_version %d < majority %d",
					blockNum, hdr.Witness, w.RunningVersion, ws.MajorityVersion)
			}
		}
	}

	return nil
}

// verifyBlockSignature recovers the pubkey from the 65-byte witness
// signature over SHA256(serialized block_header bytes) and asserts it
// matches the producing witness's BlockSigningKey on chain. Mirrors
// hived's validate_block_header (database.cpp:2887): if the recovered
// public_key disagrees with witness_object.signing_key, the block is
// rejected.
//
// rawBlock is the full block payload starting at the unsigned header;
// hdrLen is the number of bytes consumed by UnmarshalSignedHeader, so
// rawBlock[:hdrLen-block.SignatureSize] is the unsigned header bytes
// hived feeds into SHA256 to derive the digest.
func verifyBlockSignature(s *state.Session, hdr *block.SignedHeader, rawBlock []byte, hdrLen int, blockNum uint32) error {
	if hdrLen < block.SignatureSize {
		return fmt.Errorf("verify_block_signature %d: header too short (%d bytes, signature alone is %d)",
			blockNum, hdrLen, block.SignatureSize)
	}
	unsignedHeader := rawBlock[:hdrLen-block.SignatureSize]
	digest := block.BlockSigningDigest(unsignedHeader)

	var sig crypto.CompactSignature
	copy(sig[:], hdr.WitnessSignature[:])

	got, err := crypto.RecoverPublicKey(digest, sig)
	if err != nil {
		return fmt.Errorf("verify_block_signature %d: recover: %w", blockNum, err)
	}
	w, err := objects.GetWitness(s, auth.AccountName(hdr.Witness))
	if err != nil {
		return fmt.Errorf("verify_block_signature %d: witness %q: %w", blockNum, hdr.Witness, err)
	}
	if w.BlockSigningKey.IsZero() {
		return fmt.Errorf("verify_block_signature %d: witness %q has no signing_key (disabled)",
			blockNum, hdr.Witness)
	}
	if got != w.BlockSigningKey {
		return fmt.Errorf("verify_block_signature %d: signing key mismatch for %q (recovered=%s, expected=%s)",
			blockNum, hdr.Witness, got.EncodeBase58(), w.BlockSigningKey.EncodeBase58())
	}
	return nil
}

// validateMerkleRoot rebuilds the merkle root from the transactions in
// rawBlock[hdrLen:] and compares with hdr.TransactionMerkleRoot.
func validateMerkleRoot(hdr *block.SignedHeader, rawBlock []byte, hdrLen int, blockNum uint32) error {
	body := rawBlock[hdrLen:]
	r := fcraw.NewReader(body)
	count, err := r.ReadVarUint()
	if err != nil {
		return fmt.Errorf("validate_block %d: tx count: %w", blockNum, err)
	}
	if count == 0 {
		var zero [20]byte
		if hdr.TransactionMerkleRoot != zero {
			return fmt.Errorf("validate_block %d: empty-block merkle root mismatch", blockNum)
		}
		return nil
	}
	digests := make([]tx.Digest, 0, count)
	for i := uint32(0); i < count; i++ {
		txRes, perr := tx.ReadSignedTransaction(r)
		if perr != nil {
			return fmt.Errorf("validate_block %d: tx %d parse: %w", blockNum, i, perr)
		}
		// txRes positions are relative to the body buffer (since we
		// passed body=rawBlock[hdrLen:] to fcraw.NewReader). The signed
		// span is [UnsignedStart, SignedEnd) within body.
		signed := body[txRes.UnsignedStart:txRes.SignedEnd]
		digests = append(digests, tx.MerkleDigestFromBytes(signed))
	}
	got := tx.MerkleRoot(digests)
	if !block.MerkleRootAccepted(blockNum, got, hdr.TransactionMerkleRoot) {
		return fmt.Errorf("validate_block %d: merkle root mismatch: got %x expected %x",
			blockNum, got[:8], hdr.TransactionMerkleRoot[:8])
	}
	return nil
}
