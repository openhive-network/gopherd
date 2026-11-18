package witness

import (
	"crypto/sha256"
	"testing"
	"time"

	"gopherd/internal/chain"
	"gopherd/internal/crypto"
	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
	"gopherd/internal/protocol/tx"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

// TestSignFastConfirmRoundTrip: the approval decodes as a fast-confirm for
// the block, references the given block by TaPoS, and its signature recovers
// to the producer's key under the mainnet chain id.
func TestSignFastConfirmRoundTrip(t *testing.T) {
	seed := sha256.Sum256([]byte("fast-confirm-sign"))
	priv, err := crypto.PrivateKeyFromBytes(seed[:])
	if err != nil {
		t.Fatal(err)
	}
	prod := &Producer{Account: "howo", PrivateKey: priv, PublicKey33: priv.PublicKey()}
	var approved, ref block.ID
	approved[3], approved[10] = 7, 0xaa
	ref[2], ref[3], ref[4], ref[5] = 1, 2, 0x11, 0x22

	body, err := signFastConfirm(prod, approved, ref, 1234)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tx.ReadSignedTransaction(fcraw.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	st := res.SignedTx
	if !chain.IsFastConfirmTx(&st) {
		t.Fatal("not recognised as a fast-confirm")
	}
	a := st.Operations[0].AsWitnessBlockApprove()
	if string(a.Witness) != "howo" || block.ID(a.BlockID) != approved {
		t.Fatalf("approve op = %s/%x", a.Witness, a.BlockID)
	}
	if st.RefBlockNum != uint16(ref.Num()) || st.RefBlockPrefix != 0x2211 || st.Expiration != 1234 {
		t.Fatalf("tapos/expiration = %d/%x/%d", st.RefBlockNum, st.RefBlockPrefix, st.Expiration)
	}
	digest := crypto.SHA256Of2(network.ChainID[:], body[res.UnsignedStart:res.UnsignedEnd])
	key, err := crypto.RecoverPublicKey(digest, crypto.CompactSignature(st.Signatures[0]))
	if err != nil || key != prod.PublicKey33 {
		t.Fatalf("signature recovers to %v (%v), want the producer's key", key, err)
	}
}

// TestBuildFastConfirmsOffOrUndecodable: nothing is signed while the switch
// is off, nor for a block whose header does not decode.
func TestBuildFastConfirmsOffOrUndecodable(t *testing.T) {
	p := newMempoolPlugin(t)
	if got := p.BuildFastConfirms([]byte{1, 2, 3}, 10, time.Now()); got != nil {
		t.Fatal("signed with FastConfirm off")
	}
	p.FastConfirm = true
	if got := p.BuildFastConfirms([]byte{1, 2, 3}, 10, time.Now()); got != nil {
		t.Fatal("signed for an undecodable block")
	}
}

// hived 86e15d046: an approval of a block more than 3 above our head, which
// we cannot hold, is refused (so not stored or relayed); one just above the
// head is kept, its block may simply not have arrived yet.
func TestFastConfirmFarAboveHeadRefused(t *testing.T) {
	seed := sha256.Sum256([]byte("fast-confirm-far-ahead"))
	priv, err := crypto.PrivateKeyFromBytes(seed[:])
	if err != nil {
		t.Fatal(err)
	}
	cdb, db := newSigningFixture(t, priv)
	s := state.NewSession(db)
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		t.Fatal(err)
	}
	dgpo.HeadBlockNumber = hardfork.MainnetActivationBlock[hardfork.HF26] + 100
	objects.PutDGPO(s, &dgpo)
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}
	if !containsAccount(chain.IrreversibilitySchedule(state.NewSession(db), dgpo.HeadBlockNumber), genesis.MainnetInitminer) {
		t.Skip("fixture schedule does not hold initminer")
	}
	prod := &Producer{Account: genesis.MainnetInitminer, PrivateKey: priv, PublicKey33: priv.PublicKey()}
	approve := func(num uint32) error {
		var id block.ID
		id[0], id[1], id[2], id[3] = byte(num>>24), byte(num>>16), byte(num>>8), byte(num)
		id[10] = 0xab
		body, err := signFastConfirm(prod, id, block.ID{}, dgpo.Time+60)
		if err != nil {
			t.Fatal(err)
		}
		return cdb.ProcessFastConfirm(body)
	}
	if err := approve(dgpo.HeadBlockNumber + 10); err == nil {
		t.Fatal("approval of an unknown block 10 above head was accepted")
	}
	if err := approve(dgpo.HeadBlockNumber + 2); err != nil {
		t.Fatalf("approval 2 above head refused: %v", err)
	}
}

func containsAccount(names []auth.AccountName, want string) bool {
	for _, n := range names {
		if string(n) == want {
			return true
		}
	}
	return false
}
