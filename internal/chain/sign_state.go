package chain

import (
	"gopherd/internal/crypto"
	"gopherd/internal/protocol/auth"
)

// HIVE_MAX_SIG_CHECK_DEPTH = 2 (libraries/protocol/include/hive/protocol/
// config.hpp). Limits recursion through account_auths so a pathological
// authority graph can't OOM the verifier.
const maxSigCheckDepth = 2

// HIVE_MAX_AUTHORITY_MEMBERSHIP — per-authority limit on
// len(key_auths)+len(account_auths). Hived defaults to 40 (config.hpp:375);
// HF20+ enforced via verify_authority's max_membership parameter
// (database.cpp:2603-2604). Gopherd applies unconditionally — pre-HF20
// txs don't exceed 40 in practice on mainnet.
const maxAuthorityMembership = 40

// HIVE_MAX_SIG_CHECK_ACCOUNTS — cumulative cap on account_auths followed
// during a single verify (config.hpp:358 = 125). Same HF20+ caveat;
// applied unconditionally for parity defense.
const maxSigCheckAccounts = 125

// authorityFetcher returns the {owner, active, posting} authority for an
// account name. Returned pointers may be nil if the account doesn't have
// the requested authority registered (treated as a satisfaction failure).
type authorityFetcher interface {
	OwnerAuthority(name auth.AccountName) (*auth.Authority, error)
	ActiveAuthority(name auth.AccountName) (*auth.Authority, error)
	PostingAuthority(name auth.AccountName) (*auth.Authority, error)
}

// signState mirrors hive::protocol::sign_state. It threads the set of
// recovered signing keys + memoization through the recursive
// check_authority walk.
type signState struct {
	availableKeys map[crypto.PublicKey]struct{}
	usedKeys      map[crypto.PublicKey]bool
	fetcher       authorityFetcher
	visited       map[string]bool
	// accountAuthsFollowed is the cumulative count of account_auths
	// entries we've recursed into during the current verify. Capped at
	// maxSigCheckAccounts to match hived's HIVE_MAX_SIG_CHECK_ACCOUNTS.
	accountAuthsFollowed int
}

// newSignState constructs an empty sign-state seeded with `keys` (the
// public keys recovered from each tx signature).
func newSignState(keys []crypto.PublicKey, fetcher authorityFetcher) *signState {
	avail := make(map[crypto.PublicKey]struct{}, len(keys))
	for _, k := range keys {
		avail[k] = struct{}{}
	}
	return &signState{
		availableKeys: avail,
		usedKeys:      make(map[crypto.PublicKey]bool, len(keys)),
		fetcher:       fetcher,
		visited:       make(map[string]bool, 4),
	}
}

// checkAuthority returns true if the supplied authority is satisfied by
// the recovered keys + (recursive, depth≤max) account_auths under the
// supplied authority-level fetcher.
func (s *signState) checkAuthority(level AuthorityLevel, name auth.AccountName, depth int) bool {
	if depth > maxSigCheckDepth {
		return false
	}
	if s.visited[string(name)] {
		return false
	}
	s.visited[string(name)] = true
	defer delete(s.visited, string(name))

	authPtr, err := s.fetchAuthority(level, name)
	if err != nil || authPtr == nil {
		return false
	}
	return s.satisfies(authPtr, level, depth)
}

// satisfies returns true if `a`'s weight_threshold is met by the
// signing-key + account-auth combination available in s.
func (s *signState) satisfies(a *auth.Authority, level AuthorityLevel, depth int) bool {
	if a == nil || a.WeightThreshold == 0 {
		// hived treats threshold==0 as "trivially unsatisfiable" — only
		// real authorities can be satisfied.
		return false
	}
	// HIVE_MAX_AUTHORITY_MEMBERSHIP — per-authority cap on the sum of
	// key_auths + account_auths. Pathological authorities are rejected.
	if len(a.KeyAuths)+len(a.AccountAuths) > maxAuthorityMembership {
		return false
	}
	total := uint64(0)
	for _, ka := range a.KeyAuths {
		if _, ok := s.availableKeys[ka.Key]; ok {
			s.usedKeys[ka.Key] = true
			total += uint64(ka.Weight)
			if total >= uint64(a.WeightThreshold) {
				return true
			}
		}
	}
	// Mirror C++ transaction_util.cpp:119: when verifying OWNER- or
	// ACTIVE-required ops, the sign_state's recursion fetcher is set
	// to get_active BEFORE the required_active / required_owner walks
	// begin. So even though the TOP-LEVEL authority of an owner-required
	// op is the owner auth, sub-accounts referenced via account_auths
	// are looked up at the ACTIVE level — gopherd previously fetched
	// owner-of-subaccount which would accept signatures from a key only
	// on the sub-account's owner key (stricter than hived) or reject a
	// valid sig that's only on the sub-account's active key. For
	// POSTING-required, the sign_state stays at the POSTING fetcher
	// throughout (constructor at C++ line 57).
	subLevel := level
	if level == AuthorityOwner {
		subLevel = AuthorityActive
	}
	for _, aa := range a.AccountAuths {
		// Mirror hive sign_state.hpp:216 — at the recursion limit, account_auths
		// are NOT followed AND NOT counted (the depth check precedes the
		// account_auth_count bump). gopherd previously incremented
		// accountAuthsFollowed and recursed one level deeper before the depth
		// guard in checkAuthority rejected it, so the depth-(max+1) edges that
		// hived skips were still counted. On a deeply-delegated authority those
		// spurious counts blow past HIVE_MAX_SIG_CHECK_ACCOUNTS and spuriously
		// fail a valid signature: block 107,990,471 tx 18 (a superslayer0040
		// vote signed by rafiki.vote's posting key — rafiki.vote is the 12th of
		// 16 posting account_auths, and earlier apps like cryptobrewmaster carry
		// ~17 sub-auths each, so the pre-fix count exceeded 125 before reaching
		// rafiki.vote → "missing posting authority").
		if depth >= maxSigCheckDepth {
			continue
		}
		// HIVE_MAX_SIG_CHECK_ACCOUNTS — cumulative cap. A pathological
		// graph with >125 account_auths short-circuits as unsatisfied.
		s.accountAuthsFollowed++
		if s.accountAuthsFollowed > maxSigCheckAccounts {
			return false
		}
		if s.checkAuthority(subLevel, aa.Account, depth+1) {
			total += uint64(aa.Weight)
			if total >= uint64(a.WeightThreshold) {
				return true
			}
		}
	}
	return false
}

func (s *signState) fetchAuthority(level AuthorityLevel, name auth.AccountName) (*auth.Authority, error) {
	switch level {
	case AuthorityOwner:
		return s.fetcher.OwnerAuthority(name)
	case AuthorityActive:
		return s.fetcher.ActiveAuthority(name)
	case AuthorityPosting:
		return s.fetcher.PostingAuthority(name)
	}
	return nil, nil
}

// signedByKey reports whether the transaction carries a signature from this
// exact key, and marks it CONSUMED.
//
// Mirrors C++ sign_state::signed_by (used at transaction_util.cpp:181 for the
// witness category). Marking it used is not incidental: without it the key
// would look unconsumed to the pre-HF28 remove_unused_signatures assert and a
// correctly-signed witness_block_approve would be rejected as carrying an
// irrelevant signature.
func (s *signState) signedByKey(k crypto.PublicKey) bool {
	if _, ok := s.availableKeys[k]; !ok {
		return false
	}
	s.usedKeys[k] = true
	return true
}
