package chain

import (
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

func keyAuthority(k crypto.PublicKey) auth.Authority {
	return auth.Authority{WeightThreshold: 1, KeyAuths: []auth.KeyAuth{{Key: k, Weight: 1}}}
}

func singleOpTx(tag op.Tag, p unsafe.Pointer) *tx.SignedTransaction {
	stx := &tx.SignedTransaction{}
	stx.Operations = []op.Op{op.OpFromPtr(tag, p)}
	return stx
}

// TestRecoverAccountRequiresEmbeddedAuthorities pins hived's `other`
// category for recover_account (hive_operations.hpp:851-855 →
// transaction_util.cpp:124-127): the tx signatures must satisfy BOTH
// new_owner_authority and recent_owner_authority.
//
// Both authorities are public data — the open request and
// owner_authority_history — so before this check an UNSIGNED recover_account
// for any account with an open request passed verification.
func TestRecoverAccountRequiresEmbeddedAuthorities(t *testing.T) {
	newKey := crypto.PublicKey{1, 1, 1}
	recentKey := crypto.PublicKey{2, 2, 2}
	s := state.NewSession(state.OpenMem())
	objects.EnsureDGPOStore(s)
	objects.PutDGPO(s, &objects.DynamicGlobalProperty{HeadBlockNumber: 100})

	mk := func() *tx.SignedTransaction {
		return singleOpTx(op.TagRecoverAccount, unsafe.Pointer(&op.RecoverAccount{
			AccountToRecover:     "victim",
			NewOwnerAuthority:    keyAuthority(newKey),
			RecentOwnerAuthority: keyAuthority(recentKey),
		}))
	}

	if err := VerifyAuthorityWithKeys(s, mk(), nil); err == nil {
		t.Fatal("unsigned recover_account was accepted")
	}
	if err := VerifyAuthorityWithKeys(s, mk(), []crypto.PublicKey{newKey}); err == nil {
		t.Fatal("recover_account signed only by the new owner key was accepted; recent_owner_authority must be satisfied too")
	}
	if err := VerifyAuthorityWithKeys(s, mk(), []crypto.PublicKey{recentKey}); err == nil {
		t.Fatal("recover_account signed only by the recent owner key was accepted; new_owner_authority must be satisfied too")
	}
	if err := VerifyAuthorityWithKeys(s, mk(), []crypto.PublicKey{newKey, recentKey}); err != nil {
		t.Fatalf("recover_account signed by both embedded authorities rejected: %v", err)
	}
}

// TestWitnessSetPropertiesRequiresSigningKey pins the `other` authority for
// witness_set_properties (hive_operations.hpp:475-489): authority(1, props["key"], 1).
// The evaluator compares props["key"] with the stored signing key, but that
// is public; only the signature proves control of it.
func TestWitnessSetPropertiesRequiresSigningKey(t *testing.T) {
	signing := crypto.PublicKey{3, 3, 3}
	s := state.NewSession(state.OpenMem())
	objects.EnsureDGPOStore(s)
	objects.PutDGPO(s, &objects.DynamicGlobalProperty{HeadBlockNumber: 100})

	mk := func(keyBlob []byte) *tx.SignedTransaction {
		return singleOpTx(op.TagWitnessSetProperties, unsafe.Pointer(&op.WitnessSetProperties{
			Owner: "gtg",
			Props: []op.WitnessProperty{{Key: "key", Value: keyBlob}},
		}))
	}

	if err := VerifyAuthorityWithKeys(s, mk(signing[:]), nil); err == nil {
		t.Fatal("unsigned witness_set_properties was accepted")
	}
	if err := VerifyAuthorityWithKeys(s, mk(signing[:]), []crypto.PublicKey{{9, 9, 9}}); err == nil {
		t.Fatal("witness_set_properties signed by an unrelated key was accepted")
	}
	if err := VerifyAuthorityWithKeys(s, mk(signing[:]), []crypto.PublicKey{signing}); err != nil {
		t.Fatalf("witness_set_properties signed by the signing key rejected: %v", err)
	}
	// hived's fc::raw::unpack_from_vector throws on a truncated key blob.
	if err := VerifyAuthorityWithKeys(s, mk(signing[:10]), []crypto.PublicKey{signing}); err == nil {
		t.Fatal("witness_set_properties with a truncated 'key' blob was accepted")
	}
}

// TestHF28DisablesRoleUpgrade pins transaction_util.cpp:84-150: with
// HIVE_HARDFORK_1_28_ALLOW_STRICT_AND_MIXED_AUTHORITIES hived checks the
// required authority alone; owner-for-active and active/owner-for-posting
// substitution is the pre-HF28 `else` branch.
func TestHF28DisablesRoleUpgrade(t *testing.T) {
	ownerKey := crypto.PublicKey{1, 1, 1}
	activeKey := crypto.PublicKey{2, 2, 2}
	postingKey := crypto.PublicKey{3, 3, 3}

	hf28Block := hardfork.MainnetActivationBlock[hardfork.HF28]
	if hf28Block == 0 {
		t.Skip("testnet build: no pre-HF28 era exists")
	}

	session := func(head uint32) *state.Session {
		s := state.NewSession(state.OpenMem())
		objects.EnsureDGPOStore(s)
		objects.PutDGPO(s, &objects.DynamicGlobalProperty{HeadBlockNumber: head})
		objects.PutAccountAuthority(s, &objects.AccountAuthority{
			Account: "alice",
			Owner:   keyAuthority(ownerKey),
			Active:  keyAuthority(activeKey),
			Posting: keyAuthority(postingKey),
		})
		return s
	}
	transfer := func() *tx.SignedTransaction {
		return singleOpTx(op.TagTransfer, unsafe.Pointer(&op.Transfer{From: "alice", To: "bob"}))
	}
	vote := func() *tx.SignedTransaction {
		return singleOpTx(op.TagVote, unsafe.Pointer(&op.Vote{Voter: "alice", Author: "bob", Permlink: "p"}))
	}

	t.Run("pre-HF28 owner may sign for active, active may sign for posting", func(t *testing.T) {
		if err := VerifyAuthorityWithKeys(session(hf28Block-1), transfer(), []crypto.PublicKey{ownerKey}); err != nil {
			t.Fatalf("pre-HF28 owner-signed transfer rejected: %v", err)
		}
		if err := VerifyAuthorityWithKeys(session(hf28Block-1), vote(), []crypto.PublicKey{activeKey}); err != nil {
			t.Fatalf("pre-HF28 active-signed vote rejected: %v", err)
		}
	})
	t.Run("post-HF28 the exact authority is required", func(t *testing.T) {
		if err := VerifyAuthorityWithKeys(session(hf28Block+1), transfer(), []crypto.PublicKey{ownerKey}); err == nil {
			t.Fatal("post-HF28 owner-signed transfer accepted; hived raises tx_missing_active_auth")
		}
		if err := VerifyAuthorityWithKeys(session(hf28Block+1), vote(), []crypto.PublicKey{activeKey}); err == nil {
			t.Fatal("post-HF28 active-signed vote accepted; hived raises tx_missing_posting_auth")
		}
		if err := VerifyAuthorityWithKeys(session(hf28Block+1), transfer(), []crypto.PublicKey{activeKey}); err != nil {
			t.Fatalf("post-HF28 active-signed transfer rejected: %v", err)
		}
		if err := VerifyAuthorityWithKeys(session(hf28Block+1), vote(), []crypto.PublicKey{postingKey}); err != nil {
			t.Fatalf("post-HF28 posting-signed vote rejected: %v", err)
		}
	})
}
