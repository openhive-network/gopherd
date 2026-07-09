package chain

import (
	"testing"

	"hived-go/internal/crypto"
	"hived-go/internal/protocol/auth"
)

// mapFetcher is a trivial authorityFetcher backed by a posting-authority map.
type mapFetcher struct{ posting map[string]*auth.Authority }

func (m mapFetcher) OwnerAuthority(n auth.AccountName) (*auth.Authority, error)  { return nil, nil }
func (m mapFetcher) ActiveAuthority(n auth.AccountName) (*auth.Authority, error) { return nil, nil }
func (m mapFetcher) PostingAuthority(n auth.AccountName) (*auth.Authority, error) {
	return m.posting[string(n)], nil
}

// TestDeepDelegationAccountAuthCount reproduces block 107,990,471 tx 18:
// a vote by "root" whose posting authority delegates to 16 apps; the signer
// only satisfies the 12th app ("target"), and several earlier apps carry deep
// sub-delegations. Before the sign_state depth-before-count fix, the spurious
// depth-(max+1) edge counts pushed accountAuthsFollowed past 125 before
// reaching "target", failing a valid signature.
func TestDeepDelegationAccountAuthCount(t *testing.T) {
	signer := crypto.PublicKey{1, 2, 3, 4, 5}
	posting := map[string]*auth.Authority{}

	// A "fat" app: 17 account_auths to leaf accounts (each leaf has its own
	// account_auths that hived skips at max depth but gopherd used to count).
	makeFat := func(name string, n int) {
		a := &auth.Authority{WeightThreshold: 1,
			KeyAuths: []auth.KeyAuth{{Key: crypto.PublicKey{99}, Weight: 1}}}
		for i := 0; i < n; i++ {
			leaf := name + "-leaf-" + string(rune('a'+i))
			a.AccountAuths = append(a.AccountAuths, auth.AccountAuth{Account: auth.AccountName(leaf), Weight: 1})
			// Each leaf itself delegates to 6 more (depth-2 sub-auths).
			la := &auth.Authority{WeightThreshold: 1,
				KeyAuths: []auth.KeyAuth{{Key: crypto.PublicKey{98}, Weight: 1}}}
			for j := 0; j < 6; j++ {
				la.AccountAuths = append(la.AccountAuths, auth.AccountAuth{Account: auth.AccountName(leaf + "-x" + string(rune('a'+j))), Weight: 1})
				posting[leaf+"-x"+string(rune('a'+j))] = &auth.Authority{WeightThreshold: 1,
					KeyAuths: []auth.KeyAuth{{Key: crypto.PublicKey{97}, Weight: 1}}}
			}
			posting[leaf] = la
		}
		posting[name] = a
	}

	root := &auth.Authority{WeightThreshold: 1,
		KeyAuths: []auth.KeyAuth{{Key: crypto.PublicKey{50}, Weight: 1}}}
	// 11 fat apps ahead of the target, then the target holding the signer key.
	for i := 0; i < 11; i++ {
		app := "app" + string(rune('a'+i))
		// ~5 direct sub-auths per app (each with 6 depth-2 sub-auths). With the
		// depth-before-count fix the cumulative reaches ~66 at the target
		// (< 125); WITHOUT it the depth-2→3 edges (5*6 per app) push the count
		// past 125 by the 4th app, before the target is ever reached.
		makeFat(app, 5)
		root.AccountAuths = append(root.AccountAuths, auth.AccountAuth{Account: auth.AccountName(app), Weight: 1})
	}
	root.AccountAuths = append(root.AccountAuths, auth.AccountAuth{Account: "target", Weight: 1})
	posting["target"] = &auth.Authority{WeightThreshold: 1, KeyAuths: []auth.KeyAuth{{Key: signer, Weight: 1}}}
	posting["root"] = root

	s := newSignState([]crypto.PublicKey{signer}, mapFetcher{posting})
	if !s.checkAuthority(AuthorityPosting, "root", 0) {
		t.Fatal("root posting authority NOT satisfied by target's delegated key — depth-before-count regression")
	}
}
