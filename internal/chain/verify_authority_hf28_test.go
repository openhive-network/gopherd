package chain

import (
	"errors"
	"testing"
	"unsafe"

	"gopherd/internal/crypto"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// hf28AuthSession builds a minimal in-memory session with DGPO parked at
// headBlock and a single account "alice" whose ACTIVE authority is satisfied by
// exactly one key.
func hf28AuthSession(t *testing.T, headBlock uint32, activeKey crypto.PublicKey) *state.Session {
	t.Helper()
	s := state.NewSession(state.OpenMem())
	objects.EnsureDGPOStore(s)
	objects.PutDGPO(s, &objects.DynamicGlobalProperty{HeadBlockNumber: headBlock})
	objects.PutAccountAuthority(s, &objects.AccountAuthority{
		Account: "alice",
		Active: auth.Authority{
			WeightThreshold: 1,
			KeyAuths:        []auth.KeyAuth{{Key: activeKey, Weight: 1}},
		},
	})
	return s
}

// transferTx builds a tx whose single op requires alice's ACTIVE authority.
func transferTx() *tx.SignedTransaction {
	stx := &tx.SignedTransaction{}
	stx.Operations = []op.Op{
		op.OpFromPtr(op.TagTransfer, unsafe.Pointer(&op.Transfer{From: "alice", To: "bob"})),
	}
	return stx
}

// TestHF28AllowsRedundantSignatures pins the consensus behaviour behind the
// live wall at block 108,860,724, where a valid mainnet tx carried a signature
// that no required authority consumed and gopherd rejected it as
// tx_irrelevant_sig — wedging the node one block below the wall.
//
// C++ guards BOTH remove_unused_signatures asserts (transaction_util.cpp:111
// and :185) with `if( !allow_redundant_signatures )`, and database.cpp:2647
// passes has_hardfork( HIVE_HARDFORK_1_28_ALLOW_REDUNDANT_SIGNATURES ). So the
// extra signature must be REJECTED before HF28 and ACCEPTED from HF28 on.
//
// This case cannot be caught by replay: historical blocks apply with
// SkipTransactionSignatures, so VerifyAuthority short-circuits at its first
// line and never reaches this check. Only the live tip exercises it — hence
// the unit test.
func TestHF28AllowsRedundantSignatures(t *testing.T) {
	needed := crypto.PublicKey{1, 2, 3}
	redundant := crypto.PublicKey{9, 9, 9}

	hf28Block := hardfork.MainnetActivationBlock[hardfork.HF28]
	if hf28Block == 0 {
		t.Fatal("no activation block recorded for HF28")
	}

	t.Run("pre-HF28 rejects the unused signature", func(t *testing.T) {
		s := hf28AuthSession(t, hf28Block-1, needed)
		err := VerifyAuthorityWithKeys(s, transferTx(), []crypto.PublicKey{needed, redundant})
		if !errors.Is(err, ErrIrrelevantSig) {
			t.Fatalf("pre-HF28: got %v, want ErrIrrelevantSig", err)
		}
	})

	// Boundary: SetForBlock activates on `blockNum > activation` (strictly
	// greater), mirroring C++ running process_hardforks at the END of
	// apply_block. So the activation block ITSELF is still pre-HF28 and must
	// keep rejecting; the relaxation starts one block later.
	t.Run("at the activation block it is still pre-HF28", func(t *testing.T) {
		s := hf28AuthSession(t, hf28Block, needed)
		err := VerifyAuthorityWithKeys(s, transferTx(), []crypto.PublicKey{needed, redundant})
		if !errors.Is(err, ErrIrrelevantSig) {
			t.Fatalf("at activation block %d: got %v, want ErrIrrelevantSig", hf28Block, err)
		}
	})

	t.Run("post-HF28 accepts the unused signature", func(t *testing.T) {
		s := hf28AuthSession(t, hf28Block+1, needed)
		if err := VerifyAuthorityWithKeys(s, transferTx(), []crypto.PublicKey{needed, redundant}); err != nil {
			t.Fatalf("post-HF28: got %v, want nil (this is the 108,860,724 wall)", err)
		}
	})

	// The relaxation must not become "authority optional": a tx whose required
	// authority is NOT satisfied has to keep failing on both sides of HF28,
	// otherwise the fix would have traded a false reject for a false accept.
	t.Run("post-HF28 still rejects a missing authority", func(t *testing.T) {
		s := hf28AuthSession(t, hf28Block+1, needed)
		if err := VerifyAuthorityWithKeys(s, transferTx(), []crypto.PublicKey{redundant}); err == nil {
			t.Fatal("post-HF28: unsatisfied active authority was accepted")
		}
	})
}
