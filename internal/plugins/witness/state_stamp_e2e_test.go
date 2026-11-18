package witness

import (
	"crypto/sha256"
	"testing"

	"gopherd/internal/chain"
	"gopherd/internal/crypto"
	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

// stampFixture is the signing fixture with stamps already published, as the
// RC reports since HF20 leave them, and HF29 active from block 1 when hf29.
func stampFixture(t *testing.T, hf29 bool, stamps [][32]byte) (*Plugin, *Producer, *chain.Database, *state.Database) {
	t.Helper()
	if hardfork.IsScheduled(hardfork.HF29) {
		t.Skip("HF29 has a compiled-in activation block in this build")
	}
	seed := sha256.Sum256([]byte("witness-state-stamp-e2e"))
	priv, err := crypto.PrivateKeyFromBytes(seed[:])
	if err != nil {
		t.Fatal(err)
	}
	cdb, db := newSigningFixture(t, priv)
	s := state.NewSession(db)
	objects.PutStateStampData(s, &objects.StateStampData{Stamps: stamps})
	if hf29 {
		// Activation recorded in the hardfork_property_object the way the
		// witness vote leaves it, so ApplyBlock's syncDynamicHardforks keeps it.
		hpo, err := objects.GetHardforkPropertyObject(s)
		if err != nil {
			t.Fatal(err)
		}
		hpo.SetDynamicActivation(uint32(hardfork.HF29), 0)
		objects.PutHardforkPropertyObject(s, &hpo)
		hardfork.SetDynamicActivation(hardfork.HF29, 0)
		t.Cleanup(func() { hardfork.ClearDynamicActivation(hardfork.HF29) })
	}
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}
	cdb = chain.New(db)
	prod := &Producer{Account: genesis.MainnetInitminer, PrivateKey: priv, PublicKey33: priv.PublicKey()}
	return New(cdb, db, []Producer{*prod}), prod, cdb, db
}

// produceAndApply builds the next block through the plugin, applies it
// through the chain with signature checks, and returns its state stamp
// extension, if any.
func produceAndApply(t *testing.T, p *Plugin, prod *Producer, cdb *chain.Database, db *state.Database) *[20]byte {
	t.Helper()
	dgpo, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.buildBlock(prod, dgpo, dgpo.Time+network.BlockIntervalSec)
	if err != nil {
		t.Fatalf("buildBlock: %v", err)
	}
	sh, _, err := block.UnmarshalSignedHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	exts, err := block.ParseHeaderExtensions(sh.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	var stamp *[20]byte
	for i := range exts {
		if exts[i].Tag == block.HeaderExtStateStamp {
			stamp = &exts[i].StateMerkleRoot
		}
	}
	if _, err := cdb.ApplyBlock(raw, dgpo.HeadBlockNumber+1, chain.ApplyOptions{
		SkipTransactionSignatures: true,
	}, &chain.Stats{}); err != nil {
		t.Fatalf("ApplyBlock %d: %v", dgpo.HeadBlockNumber+1, err)
	}
	return stamp
}

func pendingStamps(t *testing.T, db *state.Database) [][32]byte {
	t.Helper()
	d, ok, err := objects.GetStateStampData(state.NewSession(db))
	if !ok || err != nil {
		t.Fatalf("state_stamp_data: ok=%v err=%v", ok, err)
	}
	return d.Stamps
}

// TestProducedBlockCommitsPendingStamps mirrors hived's
// state_stamp_published_before_hf29 from the producer's side
// (block_producer::add_state_stamp): the first block produced under HF29
// carries the root of every stamp published before it, in publication
// order, our own chain accepts it and the list is consumed; the next block
// carries nothing.
func TestProducedBlockCommitsPendingStamps(t *testing.T) {
	stamps := [][32]byte{sha256.Sum256([]byte("first")), sha256.Sum256([]byte("second")), sha256.Sum256([]byte("third"))}
	p, prod, cdb, db := stampFixture(t, true, stamps)

	stamp := produceAndApply(t, p, prod, cdb, db)
	if stamp == nil {
		t.Fatal("first HF29 block carries no state stamp")
	}
	if want := chain.StateStampRoot(stamps); *stamp != want {
		t.Fatalf("stamp %x, want %x", *stamp, want)
	}
	if left := pendingStamps(t, db); len(left) != 0 {
		t.Fatalf("stamps left after the stamp-carrying block: %x", left)
	}

	if stamp := produceAndApply(t, p, prod, cdb, db); stamp != nil {
		t.Fatalf("block with nothing pending carries a stamp: %x", *stamp)
	}
}

// TestProducedBlockOmitsStampBeforeHF29 — before HF29 the extension is not
// allowed, and the stamps keep accumulating for the first HF29 block.
func TestProducedBlockOmitsStampBeforeHF29(t *testing.T) {
	stamps := [][32]byte{sha256.Sum256([]byte("first"))}
	p, prod, cdb, db := stampFixture(t, false, stamps)
	if stamp := produceAndApply(t, p, prod, cdb, db); stamp != nil {
		t.Fatalf("pre-HF29 block carries a stamp: %x", *stamp)
	}
	if left := pendingStamps(t, db); len(left) != 1 || left[0] != stamps[0] {
		t.Fatalf("pre-HF29 block changed the list: %x", left)
	}
}

// TestProducerRefusesIncompleteStampList — a state that cannot compute the
// root must not produce: whatever it signed would be refused everywhere.
func TestProducerRefusesIncompleteStampList(t *testing.T) {
	p, prod, _, db := stampFixture(t, true, [][32]byte{{1}})
	s := state.NewSession(db)
	objects.PutStateStampData(s, &objects.StateStampData{Stamps: [][32]byte{{1}}, Incomplete: true})
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}
	dgpo, err := objects.GetDGPO(state.NewSession(db))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.buildBlock(prod, dgpo, dgpo.Time+network.BlockIntervalSec); err == nil {
		t.Fatal("built a block over an incomplete stamp list")
	}
}
