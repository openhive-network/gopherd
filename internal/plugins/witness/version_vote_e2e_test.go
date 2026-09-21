package witness

import (
	"crypto/sha256"
	"testing"

	"gopherd/internal/chain"
	"gopherd/internal/crypto"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/network"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

// TestProducedBlockSetsRunningVersion is the whole point of the extension,
// end to end: produce a block through the real plugin, apply it through the
// real chain, and check that the producing witness's running_version moved.
//
// Nothing in gopherd wrote running_version before this. The apply side
// (processHeaderExtensions) was always there — it just never had an extension
// to apply, because every block we produced carried the bare 0x00 marker. So
// a witness that had never produced kept running_version 0, and
// validate_block rejects a block whose producer is below
// current_hardfork_version: gopherd could not bootstrap a new witness.
func TestProducedBlockSetsRunningVersion(t *testing.T) {
	seed := sha256.Sum256([]byte("witness-version-extension-e2e"))
	priv, err := crypto.PrivateKeyFromBytes(seed[:])
	if err != nil {
		t.Fatal(err)
	}
	cdb, db := newSigningFixture(t, priv)

	// Genesis leaves initminer's running_version at 0 — the bootstrap case.
	before, err := objects.GetWitness(state.NewSession(db), auth.AccountName(genesis.MainnetInitminer))
	if err != nil {
		t.Fatal(err)
	}
	if before.RunningVersion == network.BlockchainVersionNum {
		t.Skip("fixture already reports this version; nothing to prove")
	}

	prod := &Producer{
		Account:     genesis.MainnetInitminer,
		PrivateKey:  priv,
		PublicKey33: priv.PublicKey(),
	}
	plugin := New(cdb, db, []Producer{*prod})

	dgpo, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatal(err)
	}
	rawBlock, err := plugin.buildBlock(prod, dgpo, uint32(network.GenesisTimeUnix+3))
	if err != nil {
		t.Fatalf("buildBlock: %v", err)
	}

	// The extension has to be on the wire, not just in our intent.
	sh, _, err := block.UnmarshalSignedHeader(rawBlock)
	if err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	exts, err := block.ParseHeaderExtensions(sh.Extensions)
	if err != nil {
		t.Fatalf("the header we produced does not parse: %v", err)
	}
	found := false
	for _, e := range exts {
		if e.Tag == block.HeaderExtVersion && e.Version == network.BlockchainVersionNum {
			found = true
		}
	}
	if !found {
		t.Fatalf("produced block carries no version extension (extensions=%+v)", exts)
	}

	stats := chain.Stats{}
	if _, err := cdb.ApplyBlock(rawBlock, dgpo.HeadBlockNumber+1, chain.ApplyOptions{
		SkipTransactionSignatures: true,
		SkipBlockSignature:        false,
	}, &stats); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}

	after, err := objects.GetWitness(state.NewSession(db), auth.AccountName(genesis.MainnetInitminer))
	if err != nil {
		t.Fatal(err)
	}
	if after.RunningVersion != network.BlockchainVersionNum {
		t.Errorf("running_version after producing: got %#x, want %#x",
			after.RunningVersion, network.BlockchainVersionNum)
	}
}

// TestProducedBlockOmitsExtensionOnceCurrent — the steady state. hived emits an
// empty extensions set for almost every block, and re-sending a version the
// chain already records would grow every header for nothing.
func TestProducedBlockOmitsExtensionOnceCurrent(t *testing.T) {
	seed := sha256.Sum256([]byte("witness-version-extension-steady"))
	priv, err := crypto.PrivateKeyFromBytes(seed[:])
	if err != nil {
		t.Fatal(err)
	}
	cdb, db := newSigningFixture(t, priv)

	// Put the witness where a producing node would already have left it.
	s := state.NewSession(db)
	w, err := objects.GetWitness(s, auth.AccountName(genesis.MainnetInitminer))
	if err != nil {
		t.Fatal(err)
	}
	w.RunningVersion = network.BlockchainVersionNum
	objects.PutWitness(s, &w)
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}

	prod := &Producer{
		Account:     genesis.MainnetInitminer,
		PrivateKey:  priv,
		PublicKey33: priv.PublicKey(),
	}
	plugin := New(cdb, db, []Producer{*prod})
	dgpo, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatal(err)
	}
	rawBlock, err := plugin.buildBlock(prod, dgpo, uint32(network.GenesisTimeUnix+3))
	if err != nil {
		t.Fatalf("buildBlock: %v", err)
	}
	sh, _, err := block.UnmarshalSignedHeader(rawBlock)
	if err != nil {
		t.Fatal(err)
	}
	exts, err := block.ParseHeaderExtensions(sh.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range exts {
		if e.Tag == block.HeaderExtVersion {
			t.Errorf("version extension re-sent when the chain already records it: %+v", e)
		}
	}
}
