package witness

import (
	"crypto/sha256"
	"testing"
	"time"

	"gopherd/internal/chain"
	"gopherd/internal/crypto"
	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/network"
	"gopherd/internal/protocol/tx"
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
