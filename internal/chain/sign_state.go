package chain

import (
	"hived-go/internal/crypto"
	"hived-go/internal/protocol/auth"
)

// HIVE_MAX_SIG_CHECK_DEPTH = 2 (libraries/protocol/include/hive/protocol/
// config.hpp). Limits recursion through account_auths so a pathological
// authority graph can't OOM the verifier.
const maxSigCheckDepth = 2

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
	for _, aa := range a.AccountAuths {
		if s.checkAuthority(level, aa.Account, depth+1) {
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
