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
func authorityLevelFor(o op.Op) AuthorityLevel {
	switch o.Tag {
	case op.TagVote, op.TagComment, op.TagCommentOptions, op.TagDeleteComment:
		return AuthorityPosting
	case op.TagAccountUpdate, op.TagAccountUpdate2:
		// account_update touching the owner authority requires owner;
		// we coarsely classify all account_updates as active here.
		return AuthorityActive
	case op.TagRecoverAccount, op.TagChangeRecoveryAccount:
		return AuthorityOwner
	}
	// All others (transfer, transfer_to_vesting, witness_update,
	// limit_order, etc.) require active.
	return AuthorityActive
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

// RecoverTxSignerKeys recovers the public keys behind each tx.Signatures
// entry. Pure CPU work — no state access — so callers may run it in
// parallel across goroutines (the apply pipeline fans this out before
// handing the job to the single-writer applyLoop).
//
// chainID is the prefix mixed into the sig digest (zero pre-HF24,
// network.ChainID HF24+).
func RecoverTxSignerKeys(signedTx *tx.SignedTransaction, unsignedBytes []byte, chainID network.Hash32) ([]crypto.PublicKey, error) {
	digest := crypto.SHA256Of2(chainID[:], unsignedBytes)
	keys := make([]crypto.PublicKey, 0, len(signedTx.Signatures))
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
