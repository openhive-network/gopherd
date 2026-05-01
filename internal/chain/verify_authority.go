package chain

import (
	"errors"

	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
)

// AuthorityLevel is a coarse categorization of which authority a given
// operation requires. Mirrors hived's get_required_*_authorities() per-op
// methods.
type AuthorityLevel int

const (
	AuthorityActive AuthorityLevel = iota
	AuthorityOwner
	AuthorityPosting
)

// RequiredAuthorities walks the operations in `signedTx` and returns
// the union of accounts whose authority must sign, grouped by level.
// Mirrors transaction::get_required_authorities (libraries/protocol/
// transaction.cpp:122).
//
// For replay this is informational only (we skip verification). For
// live sync / witness production, callers verify each (account,
// level) pair against the signing keys recovered from the tx
// signatures.
func RequiredAuthorities(signedTx *tx.SignedTransaction) map[AuthorityLevel]map[string]struct{} {
	out := map[AuthorityLevel]map[string]struct{}{
		AuthorityActive:  make(map[string]struct{}),
		AuthorityOwner:   make(map[string]struct{}),
		AuthorityPosting: make(map[string]struct{}),
	}
	for _, o := range signedTx.Operations {
		level := authorityLevelFor(o)
		acct := PayerOf(o)
		if acct == "" {
			continue
		}
		out[level][acct] = struct{}{}
	}
	return out
}

// authorityLevelFor classifies an op into the authority level required
// to sign it. The mapping matches hived's per-op
// get_required_*_authorities methods.
func authorityLevelFor(o op.Operation) AuthorityLevel {
	switch o.(type) {
	case op.Vote, op.Comment, op.CommentOptions, op.DeleteComment:
		return AuthorityPosting
	case op.AccountUpdate, op.AccountUpdate2:
		// account_update touching the owner authority requires owner;
		// we coarsely classify all account_updates as active here.
		return AuthorityActive
	case op.RecoverAccount, op.ChangeRecoveryAccount:
		return AuthorityOwner
	}
	// All others (transfer, transfer_to_vesting, witness_update,
	// limit_order, etc.) require active.
	return AuthorityActive
}

// VerifyAuthority is the entry point for live-mode signature
// verification. Pre-replay we always skip; this is a stub for the
// live-sync path that the P2 phase will fill in by:
//
//   1. Recovering signing public keys from each tx.signatures via secp256k1.
//   2. Looking up each required authority's key set.
//   3. Confirming the recovered keys cover the required threshold.
//
// Returns nil on accept, an error otherwise. SkipSig short-circuits
// to nil to mirror hived's skip_transaction_signatures behavior.
func VerifyAuthority(signedTx *tx.SignedTransaction, skipSig bool) error {
	if skipSig {
		return nil
	}
	// Real verification: stubbed pending P2 sigverify port.
	return errors.New("verify_authority: signature verification not yet implemented (P2 milestone)")
}
