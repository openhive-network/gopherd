package witness

import (
	"crypto/sha256"
	"strings"
	"testing"

	"hived-go/internal/chain"
	"hived-go/internal/crypto"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/network"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
)

// newSigningFixture spins up an in-memory state DB, applies genesis, then
// rewrites initminer's BlockSigningKey to the public key for `priv`. This
// way the witness plugin can produce a block as initminer signed with our
// test key, and ApplyBlock(SkipBlockSignature=false) will validate the
// signature against the on-chain BlockSigningKey we just set.
func newSigningFixture(t *testing.T, priv *crypto.PrivateKey) (*chain.Database, *state.Database) {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(dir, 16<<20)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := genesis.Apply(db); err != nil {
		t.Fatalf("genesis.Apply: %v", err)
	}

	// Patch initminer's BlockSigningKey to our test pubkey.
	s := state.NewSession(db)
	w, err := objects.GetWitness(s, auth.AccountName(genesis.MainnetInitminer))
	if err != nil {
		t.Fatalf("GetWitness initminer: %v", err)
	}
	w.BlockSigningKey = priv.PublicKey()
	objects.PutWitness(s, &w)
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}

	return chain.New(db), db
}

// TestWitnessBuildAndVerify is the end-to-end signing+verification test:
// build a block via the witness plugin signed by a test key, apply it
// with SkipBlockSignature=false, expect success.
func TestWitnessBuildAndVerify(t *testing.T) {
	seed := sha256.Sum256([]byte("witness-signing-e2e"))
	priv, err := crypto.PrivateKeyFromBytes(seed[:])
	if err != nil {
		t.Fatalf("PrivateKeyFromBytes: %v", err)
	}

	cdb, db := newSigningFixture(t, priv)

	prod := &Producer{
		Account:     genesis.MainnetInitminer,
		PrivateKey:  priv,
		PublicKey33: priv.PublicKey(),
	}
	plugin := New(cdb, db, []Producer{*prod})

	// Pull DGPO and build a block at GenesisTime+3s.
	sess := state.NewSession(db)
	dgpo, err := objects.GetDGPO(sess)
	if err != nil {
		t.Fatalf("GetDGPO: %v", err)
	}
	blockTime := uint32(network.GenesisTimeUnix + 3)
	rawBlock, err := plugin.buildBlock(prod, dgpo, blockTime)
	if err != nil {
		t.Fatalf("buildBlock: %v", err)
	}
	if len(rawBlock) < 65 {
		t.Fatalf("rawBlock too short: %d", len(rawBlock))
	}

	// Apply with full signature verification.
	stats := chain.Stats{}
	if _, err := cdb.ApplyBlock(rawBlock, dgpo.HeadBlockNumber+1, chain.ApplyOptions{
		Sync:                      false,
		SkipTransactionSignatures: true,
		SkipBlockSignature:        false, // engages verifyBlockSignature
	}, &stats); err != nil {
		t.Fatalf("ApplyBlock with SkipBlockSignature=false: %v", err)
	}
}

// TestWitnessRejectsBadSignature mutates one byte of the witness signature
// and confirms ApplyBlock rejects the block.
func TestWitnessRejectsBadSignature(t *testing.T) {
	seed := sha256.Sum256([]byte("witness-signing-bad"))
	priv, err := crypto.PrivateKeyFromBytes(seed[:])
	if err != nil {
		t.Fatalf("PrivateKeyFromBytes: %v", err)
	}

	cdb, db := newSigningFixture(t, priv)

	prod := &Producer{
		Account:     genesis.MainnetInitminer,
		PrivateKey:  priv,
		PublicKey33: priv.PublicKey(),
	}
	plugin := New(cdb, db, []Producer{*prod})

	sess := state.NewSession(db)
	dgpo, err := objects.GetDGPO(sess)
	if err != nil {
		t.Fatalf("GetDGPO: %v", err)
	}
	rawBlock, err := plugin.buildBlock(prod, dgpo, uint32(network.GenesisTimeUnix+3))
	if err != nil {
		t.Fatalf("buildBlock: %v", err)
	}

	// Locate the 65-byte signature: it sits at the END of the unsigned
	// header bytes and BEFORE the varint tx_count. UnmarshalSignedHeader
	// would tell us the exact offset, but we don't need precision — flip
	// a byte deep inside the r component (which always lands at
	// header_end - 32) and the recovery will fail.
	mutated := append([]byte(nil), rawBlock...)
	// Find a deterministic offset inside the signature region: walk back
	// from end-of-tx-section. We know there are no transactions, so the
	// tail is [signature(65) || varint(0)=1 byte]. Mutating the first
	// byte of signature (recovery id at hdr_end - 65) is the easiest.
	sigOffset := len(mutated) - 65 - 1 // -1 for the trailing varint(0)
	if sigOffset < 0 {
		t.Fatalf("rawBlock too short: %d", len(mutated))
	}
	// Flip the recovery-id byte. RecoverPublicKey will either fail or
	// recover the wrong pubkey; either way, verifyBlockSignature rejects.
	mutated[sigOffset] ^= 0x01

	stats := chain.Stats{}
	_, err = cdb.ApplyBlock(mutated, dgpo.HeadBlockNumber+1, chain.ApplyOptions{
		Sync:                      false,
		SkipTransactionSignatures: true,
		SkipBlockSignature:        false,
	}, &stats)
	if err == nil {
		t.Fatal("ApplyBlock must reject a block with a corrupted signature")
	}
	if !strings.Contains(err.Error(), "verify_block_signature") &&
		!strings.Contains(err.Error(), "signing key") &&
		!strings.Contains(err.Error(), "recover") {
		t.Fatalf("unexpected error (want signature-verification failure): %v", err)
	}
}

// TestWitnessRejectsWrongSigner builds a block signed by ONE key but with
// the on-chain BlockSigningKey set to a DIFFERENT key. ApplyBlock must
// detect the mismatch.
func TestWitnessRejectsWrongSigner(t *testing.T) {
	// On-chain BlockSigningKey corresponds to keyA, but we sign with keyB.
	seedA := sha256.Sum256([]byte("witness-signing-wrong-A"))
	keyA, _ := crypto.PrivateKeyFromBytes(seedA[:])
	seedB := sha256.Sum256([]byte("witness-signing-wrong-B"))
	keyB, _ := crypto.PrivateKeyFromBytes(seedB[:])

	cdb, db := newSigningFixture(t, keyA)

	prod := &Producer{
		Account:     genesis.MainnetInitminer,
		PrivateKey:  keyB,
		PublicKey33: keyB.PublicKey(),
	}
	plugin := New(cdb, db, []Producer{*prod})

	sess := state.NewSession(db)
	dgpo, err := objects.GetDGPO(sess)
	if err != nil {
		t.Fatalf("GetDGPO: %v", err)
	}
	rawBlock, err := plugin.buildBlock(prod, dgpo, uint32(network.GenesisTimeUnix+3))
	if err != nil {
		t.Fatalf("buildBlock: %v", err)
	}

	stats := chain.Stats{}
	_, err = cdb.ApplyBlock(rawBlock, dgpo.HeadBlockNumber+1, chain.ApplyOptions{
		Sync:                      false,
		SkipTransactionSignatures: true,
		SkipBlockSignature:        false,
	}, &stats)
	if err == nil {
		t.Fatal("ApplyBlock must reject a block signed by the wrong key")
	}
	if !strings.Contains(err.Error(), "signing key mismatch") {
		t.Fatalf("expected signing-key-mismatch error, got: %v", err)
	}
}
