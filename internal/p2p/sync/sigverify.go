package sync

import (
	"hived-go/internal/crypto"
	"hived-go/internal/protocol/block"
)

// RecoverBlockSigner parses the signed_block header, derives the witness
// signing digest, and recovers the public key from the witness signature.
// Returns the zero PublicKey on any parse or recovery error — callers
// (handleBlock) treat that as "fall through to chain.ApplyBlock's own
// recovery path", which will reproduce the same failure with a clean
// error message.
//
// This is the look-ahead pre-recovery hook called from per-peer receive
// goroutines. With N connected peers we get N-way parallelism on the
// expensive secp256k1 recovery, leaving the single applyLoop free to
// focus on the actual state mutation. Without this pre-recovery, sig
// verification was the dominant CPU cost in the apply path
// (~47 % of CPU samples on a fast catch-up).
func RecoverBlockSigner(rawBlock []byte) crypto.PublicKey {
	hdr, hdrLen, err := block.UnmarshalSignedHeader(rawBlock)
	if err != nil {
		return crypto.PublicKey{}
	}
	if hdrLen < block.SignatureSize {
		return crypto.PublicKey{}
	}
	unsignedHeader := rawBlock[:hdrLen-block.SignatureSize]
	digest := block.BlockSigningDigest(unsignedHeader)
	var sig crypto.CompactSignature
	copy(sig[:], hdr.WitnessSignature[:])
	if !sig.IsCanonical() {
		return crypto.PublicKey{}
	}
	got, err := crypto.RecoverPublicKey(digest, sig)
	if err != nil {
		return crypto.PublicKey{}
	}
	return got
}
