package chain

import (
	"testing"

	"hived-go/internal/crypto"
)

// TestOptsForOtherStripsPerBlockHints pins the scoping of the per-block
// verification hints.
//
// PreVerifiedBlockSigner and PreRecoveredTxKeys are recovered for ONE specific
// block on the p2p receive path. PushLiveBlock then passes its ApplyOptions to
// drainForward and tryReorg, which apply OTHER blocks — buffered out-of-order
// arrivals and whole branches during a reorg. Carrying the hints across meant
// verifying one block's signature against another block's recovered key:
//
//	push_live_block 108915165 failed: apply 108915156:
//	  signing key mismatch for "ocd-witness" (recovered=<differs every attempt>)
//
// The giveaway was that the recovered key CHANGED between attempts for the same
// block — signature recovery is deterministic, so a moving answer meant the
// input was not the block's own. Valid blocks were rejected at the tip whenever
// arrival order slipped, and the error blamed an innocent witness.
func TestOptsForOtherStripsPerBlockHints(t *testing.T) {
	var key crypto.PublicKey
	key[0] = 0x02
	in := ApplyOptions{
		PreVerifiedBlockSigner: key,
		PreRecoveredTxKeys:     [][]crypto.PublicKey{{key}},
		// Non-hint options must survive: they describe HOW to apply, not WHICH
		// block was pre-verified.
		Reversible:                true,
		SkipTransactionSignatures: true,
		EnforceRC:                 true,
		ValidateOwnBlock:          true,
	}
	out := optsForOther(in)

	if out.PreVerifiedBlockSigner != (crypto.PublicKey{}) {
		t.Fatal("PreVerifiedBlockSigner survived; another block's key would be used to verify this one")
	}
	if out.PreRecoveredTxKeys != nil {
		t.Fatal("PreRecoveredTxKeys survived; another block's tx keys would be used")
	}
	if !out.Reversible || !out.SkipTransactionSignatures || !out.EnforceRC || !out.ValidateOwnBlock {
		t.Fatal("non-hint options were dropped; only the per-block hints may be cleared")
	}
	// The caller's options must not be mutated — it still needs its own hints
	// for the block actually being pushed.
	if in.PreVerifiedBlockSigner != key || in.PreRecoveredTxKeys == nil {
		t.Fatal("optsForOther mutated the caller's options; the pushed block would lose its own hints")
	}
}
