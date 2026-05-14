package chain

import (
	"fmt"

	"hived-go/internal/crypto"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/network"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
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
func RequiredAuthorities(signedTx *tx.SignedTransaction) map[AuthorityLevel]map[string]struct{} {
	out := map[AuthorityLevel]map[string]struct{}{
		AuthorityActive:  make(map[string]struct{}),
		AuthorityOwner:   make(map[string]struct{}),
		AuthorityPosting: make(map[string]struct{}),
	}
	add := func(level AuthorityLevel, name string) {
		if name != "" {
			out[level][name] = struct{}{}
		}
	}
	for _, o := range signedTx.Operations {
		switch o.Tag {
		// Posting-level operations (vote, comment, etc.) — mirrors
		// the per-op get_required_posting_authorities in C++
		// libraries/protocol/hive_operations.cpp.
		case op.TagVote:
			add(AuthorityPosting, string(o.AsVote().Voter))
		case op.TagComment:
			add(AuthorityPosting, string(o.AsComment().Author))
		case op.TagCommentOptions:
			add(AuthorityPosting, string(o.AsCommentOptions().Author))
		case op.TagDeleteComment:
			add(AuthorityPosting, string(o.AsDeleteComment().Author))
		case op.TagClaimRewardBalance:
			// hive_operations.hpp:claim_reward_balance is posting-level
			// (added via add_authorities(posting)).
			add(AuthorityPosting, string(o.AsClaimRewardBalance().Account))

		// custom_json carries BOTH required_auths (active) AND
		// required_posting_auths (posting). Each is its own list.
		// hive_operations.cpp:get_required_active_authorities /
		// get_required_posting_authorities for custom_json_operation.
		case op.TagCustomJSON:
			cj := o.AsCustomJSON()
			for _, a := range cj.RequiredPostingAuths {
				add(AuthorityPosting, string(a))
			}
			for _, a := range cj.RequiredAuths {
				add(AuthorityActive, string(a))
			}

		// custom (binary, ID-only) — active-only (required_auths).
		case op.TagCustom:
			for _, a := range o.AsCustom().RequiredAuths {
				add(AuthorityActive, string(a))
			}

		// custom_binary — multi-level (owner/active/posting/account_auths).
		case op.TagCustomBinary:
			cb := o.AsCustomBinary()
			for _, a := range cb.RequiredOwnerAuths {
				add(AuthorityOwner, string(a))
			}
			for _, a := range cb.RequiredActiveAuths {
				add(AuthorityActive, string(a))
			}
			for _, a := range cb.RequiredPostingAuths {
				add(AuthorityPosting, string(a))
			}

		// account_update / account_update2: owner-level when the owner
		// authority itself is being touched (hive_operations.cpp:account_
		// update_operation::get_required_authorities), else active.
		case op.TagAccountUpdate:
			au := o.AsAccountUpdate()
			if au.Owner != nil {
				add(AuthorityOwner, string(au.Account))
			} else {
				add(AuthorityActive, string(au.Account))
			}
		case op.TagAccountUpdate2:
			au := o.AsAccountUpdate2()
			if au.Owner != nil {
				add(AuthorityOwner, string(au.Account))
			} else {
				// account_update2 with no owner change: posting auths
				// can sign if only posting/json_metadata is updated
				// (hive_evaluator_account.cpp:451+). Defaulting to
				// active is conservative; tighten if a real block
				// uses the posting-only path.
				add(AuthorityActive, string(au.Account))
			}

		// Owner-only ops where the required authority is a STATE LOOKUP
		// on the affected account.
		case op.TagChangeRecoveryAccount, op.TagDeclineVotingRights:
			add(AuthorityOwner, PayerOf(o))

		// recover_account uses get_required_authorities (an ad-hoc list
		// of TWO embedded authorities — new_owner_authority and
		// recent_owner_authority — directly from the op), NOT
		// get_required_owner_authorities on a state-stored account.
		// Mirrors C++ hive_operations.cpp:recover_account_operation::
		// get_required_authorities. The evaluator at
		// internal/chain/evaluator/recovery.go re-checks
		// recent_owner_authority against owner_authority_history at
		// apply time, so omitting the pre-flight key→account match
		// here loses some defense-in-depth but doesn't open a real
		// hole: a malformed tx still fails the evaluator gate.
		case op.TagRecoverAccount:
			// no required state-authority; leave the op for the
			// evaluator to validate.

		default:
			// Everything else (transfer, transfer_to_vesting,
			// witness_update, limit_order, claim_*, escrow_*, etc.)
			// requires active.
			add(AuthorityActive, PayerOf(o))
		}
	}
	return out
}

// VerifyAuthority is the entry point for live-mode signature
// verification. It:
//
//  1. Recovers signing public keys from each tx.Signatures via secp256k1.
//  2. Looks up each required authority via the supplied state session.
//  3. Confirms the recovered keys cover each required authority's threshold
//     (with up to HIVE_MAX_SIG_CHECK_DEPTH levels of account_auths recursion).
//
// Returns nil on accept, an error otherwise. SkipSig short-circuits to
// nil to mirror hived's skip_transaction_signatures behavior. `chainID`
// is the chain identifier that gets prepended to the unsigned tx bytes
// in the sig digest; pass network.ChainID for HF24+ and network.OldChainID
// (zero) pre-HF24.
func VerifyAuthority(s *state.Session, signedTx *tx.SignedTransaction, unsignedBytes []byte, chainID network.Hash32, skipSig bool) error {
	if skipSig {
		return nil
	}
	keys, err := RecoverTxSignerKeys(signedTx, unsignedBytes, chainID)
	if err != nil {
		return err
	}
	return VerifyAuthorityWithKeys(s, signedTx, keys)
}

// ErrDuplicateSig is returned when two signatures recover the same
// public key. Mirrors hived's tx_duplicate_sig exception
// (libraries/protocol/transaction.cpp:108, FC_ASSERT in
// get_signature_keys).
var ErrDuplicateSig = fmt.Errorf("verify_authority: duplicate signature included")

// ErrIrrelevantSig is returned when a tx carries one or more signatures
// that were not needed to satisfy any required authority. Mirrors
// hived's tx_irrelevant_sig exception (libraries/protocol/
// transaction_util.cpp:51, FC_ASSERT on !remove_unused_signatures()).
var ErrIrrelevantSig = fmt.Errorf("verify_authority: unnecessary signature(s) detected (irrelevant)")

// RecoverTxSignerKeys recovers the public keys behind each tx.Signatures
// entry. Pure CPU work — no state access — so callers may run it in
// parallel across goroutines (the apply pipeline fans this out before
// handing the job to the single-writer applyLoop).
//
// chainID is the prefix mixed into the sig digest (zero pre-HF24,
// network.ChainID HF24+).
//
// Returns ErrDuplicateSig if two distinct signatures recover the same
// public key — mirrors hived's `flat_set::insert(...).second` assertion
// in signed_transaction::get_signature_keys.
func RecoverTxSignerKeys(signedTx *tx.SignedTransaction, unsignedBytes []byte, chainID network.Hash32) ([]crypto.PublicKey, error) {
	digest := crypto.SHA256Of2(chainID[:], unsignedBytes)
	keys := make([]crypto.PublicKey, 0, len(signedTx.Signatures))
	seen := make(map[crypto.PublicKey]struct{}, len(signedTx.Signatures))
	for i, sig := range signedTx.Signatures {
		var compact crypto.CompactSignature
		copy(compact[:], sig[:])
		if !compact.IsCanonical() {
			return nil, fmt.Errorf("verify_authority: signature %d non-canonical", i)
		}
		k, err := crypto.RecoverPublicKey(digest, compact)
		if err != nil {
			return nil, fmt.Errorf("verify_authority: recover signature %d: %w", i, err)
		}
		if _, dup := seen[k]; dup {
			return nil, ErrDuplicateSig
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	return keys, nil
}

// VerifyAuthorityWithKeys is VerifyAuthority's second half: given the
// pre-recovered set of signer pubkeys, walk the required authorities
// against state and confirm each is covered. Uses no goroutines; the
// state.Session must be owned by the caller.
func VerifyAuthorityWithKeys(s *state.Session, signedTx *tx.SignedTransaction, keys []crypto.PublicKey) error {
	required := RequiredAuthorities(signedTx)
	// Hived's verify_authority does NOT enforce "len(sigs)>0" globally;
	// each required authority is checked individually. Operations like
	// pow_operation declare zero required authorities ("there is no need
	// to verify authority, the proof of work is sufficient", per
	// hive_operations.hpp:707) and legitimately ship as 0-sig txs in
	// early mainnet (every tx in block 1092..N until pow ends). Reject
	// only when a required authority cannot be satisfied below.
	if len(required[AuthorityOwner]) == 0 &&
		len(required[AuthorityActive]) == 0 &&
		len(required[AuthorityPosting]) == 0 {
		// No required authorities — nothing to satisfy. (Hived would
		// also reject "unused signatures" pre-HF28, but in practice
		// pow txs never carry sigs and we don't currently track
		// per-key consumption; skip that strictness for now.)
		return nil
	}

	fetcher := &sessionAuthFetcher{s: s}
	state := newSignState(keys, fetcher)

	// Walk required authorities by level. Each must be satisfied. Hived
	// allows higher-power authorities to substitute for lower
	// (active/owner can sign for posting; owner can sign for active),
	// modelled by retrying upward when the strict level fails.
	for _, name := range sortedSet(required[AuthorityOwner]) {
		if !state.checkAuthority(AuthorityOwner, name, 0) {
			return fmt.Errorf("verify_authority: missing owner authority for %q", name)
		}
	}
	for _, name := range sortedSet(required[AuthorityActive]) {
		if !state.checkAuthority(AuthorityActive, name, 0) {
			// Owner can substitute for active.
			if !state.checkAuthority(AuthorityOwner, name, 0) {
				return fmt.Errorf("verify_authority: missing active authority for %q", name)
			}
		}
	}
	for _, name := range sortedSet(required[AuthorityPosting]) {
		if !state.checkAuthority(AuthorityPosting, name, 0) {
			// Active or owner can substitute for posting.
			if !state.checkAuthority(AuthorityActive, name, 0) &&
				!state.checkAuthority(AuthorityOwner, name, 0) {
				return fmt.Errorf("verify_authority: missing posting authority for %q", name)
			}
		}
	}
	// Mirrors hived's `!s.remove_unused_signatures()` final assert in
	// libraries/protocol/transaction_util.cpp:102 — every signature
	// supplied must have been consumed by at least one satisfied
	// authority. Extras are surfaced as tx_irrelevant_sig.
	for _, k := range keys {
		if !state.usedKeys[k] {
			return ErrIrrelevantSig
		}
	}
	return nil
}

// sessionAuthFetcher resolves authority lookups against a state.Session
// via the AccountAuthority table.
type sessionAuthFetcher struct{ s *state.Session }

func (f *sessionAuthFetcher) OwnerAuthority(name auth.AccountName) (*auth.Authority, error) {
	a, err := objects.GetAccountAuthority(f.s, name)
	if err != nil {
		return nil, err
	}
	o := a.Owner
	return &o, nil
}
func (f *sessionAuthFetcher) ActiveAuthority(name auth.AccountName) (*auth.Authority, error) {
	a, err := objects.GetAccountAuthority(f.s, name)
	if err != nil {
		return nil, err
	}
	o := a.Active
	return &o, nil
}
func (f *sessionAuthFetcher) PostingAuthority(name auth.AccountName) (*auth.Authority, error) {
	a, err := objects.GetAccountAuthority(f.s, name)
	if err != nil {
		return nil, err
	}
	o := a.Posting
	return &o, nil
}

// sortedSet flattens a string-set into a sorted slice. Sorting makes
// errors deterministic for callers and avoids iteration-order-dependent
// behavior in the recursion's `visited` map.
func sortedSet(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Local insertion sort — small N, avoids importing sort just for this.
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && out[j-1] > out[j] {
			out[j-1], out[j] = out[j], out[j-1]
			j--
		}
	}
	return out
}
