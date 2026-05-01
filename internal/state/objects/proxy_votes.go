package objects

import (
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// MaxProxyRecursionDepth mirrors HIVE_MAX_PROXY_RECURSION_DEPTH from
// libraries/protocol/include/hive/protocol/config.hpp:235. Account A
// can proxy to B which proxies to C ... up to 4 hops; beyond that
// votes stop propagating up the chain.
const MaxProxyRecursionDepth = 4

// AdjustProxiedWitnessVotes mirrors hived's single-value variant
// `database::adjust_proxied_witness_votes(const account_object& a,
// share_type delta, int depth)` from database.cpp:1108.
//
// If `a` has a proxy, the proxy's `proxied_vsf_votes[depth]` slot
// receives `delta`, and we recurse into the proxy at depth+1 (capping
// at MaxProxyRecursionDepth — anything deeper just stops, mirroring
// the C++ comment "nested proxies are not supported, vote will not
// propagate"). If `a` has no proxy, the delta lands on every witness
// `a` directly votes for via AdjustWitnessVotesForVoter.
//
// Used whenever a voter's effective vesting weight changes — e.g.
// transfer_to_vesting (delta = +new_vests), withdraw_vesting payout
// (delta = -toWithdraw), delegate_vesting_shares (delta = ±delegated),
// account_witness_vote add/remove (delta = ±governance_vote_power),
// account_witness_proxy clear (delta = -governance_vote_power on each
// affected slot).
func AdjustProxiedWitnessVotes(s *state.Session, a *Account, delta int64) {
	adjustProxiedWitnessVotesScalar(s, a, delta, 0)
}

func adjustProxiedWitnessVotesScalar(s *state.Session, a *Account, delta int64, depth int) {
	if delta == 0 {
		return
	}
	if a.Proxy != "" {
		if depth >= MaxProxyRecursionDepth {
			return
		}
		proxy, err := GetAccount(s, a.Proxy)
		if err != nil {
			return
		}
		proxy.ProxiedVSFVotes[depth] += delta
		PutAccount(s, &proxy)
		adjustProxiedWitnessVotesScalar(s, &proxy, delta, depth+1)
		return
	}
	AdjustWitnessVotesForVoter(s, a.Name, delta)
}

// AdjustProxiedWitnessVotesArray mirrors hived's array-variant
// `database::adjust_proxied_witness_votes(const account_object& a,
// const std::array<share_type, HIVE_MAX_PROXY_RECURSION_DEPTH+1>&
// delta, int depth)` from database.cpp:1077. Used when an account
// is starting/stopping a proxy: the propagating delta carries the
// account's own direct vest in delta[0] plus its proxied_vsf_votes
// in delta[1..MaxProxyRecursionDepth].
//
// Per C++:
//
//	if (a.has_proxy()) {
//	    if (depth >= MAX_DEPTH) return;
//	    for (int i = MAX_DEPTH - depth - 1; i >= 0; --i)
//	        proxy.proxied_vsf_votes[i+depth] += delta[i];
//	    recurse(proxy, delta, depth+1);
//	} else {
//	    total = sum(delta[0..MAX_DEPTH-depth]);
//	    adjust_witness_votes(a, total);
//	}
func AdjustProxiedWitnessVotesArray(s *state.Session, a *Account, delta [MaxProxyRecursionDepth + 1]int64) {
	adjustProxiedWitnessVotesArray(s, a, delta, 0)
}

func adjustProxiedWitnessVotesArray(s *state.Session, a *Account, delta [MaxProxyRecursionDepth + 1]int64, depth int) {
	if a.Proxy != "" {
		if depth >= MaxProxyRecursionDepth {
			return
		}
		proxy, err := GetAccount(s, a.Proxy)
		if err != nil {
			return
		}
		for i := MaxProxyRecursionDepth - depth - 1; i >= 0; i-- {
			proxy.ProxiedVSFVotes[i+depth] += delta[i]
		}
		PutAccount(s, &proxy)
		adjustProxiedWitnessVotesArray(s, &proxy, delta, depth+1)
		return
	}
	var total int64
	for i := MaxProxyRecursionDepth - depth; i >= 0; i-- {
		total += delta[i]
	}
	if total != 0 {
		AdjustWitnessVotesForVoter(s, a.Name, total)
	}
}

// NullifyProxiedWitnessVotes mirrors hived's database.cpp:1131. Used
// whenever an account is about to change its proxy / lose voting
// rights: subtract the account's full propagation (own direct + each
// proxied_vsf_votes slot) from the chain.
//
// directVotePower must be the value account.get_direct_governance_vote_power()
// returns at this moment — vesting_shares pre-HF24, vesting_shares -
// sum_delayed_votes HF24+. Caller computes it.
func NullifyProxiedWitnessVotes(s *state.Session, a *Account, directVotePower int64) {
	var delta [MaxProxyRecursionDepth + 1]int64
	delta[0] = -directVotePower
	for i := 0; i < MaxProxyRecursionDepth; i++ {
		delta[i+1] = -a.ProxiedVSFVotes[i]
	}
	adjustProxiedWitnessVotesArray(s, a, delta, 0)
}

// ApplyProxyAtDepth mirrors hived's "set new proxy" path: once an
// account A switches to proxy B, we send +A's full propagation array
// up the chain (mirror of NullifyProxiedWitnessVotes with positive
// signs).
func ApplyProxyAtDepth(s *state.Session, a *Account, directVotePower int64) {
	var delta [MaxProxyRecursionDepth + 1]int64
	delta[0] = directVotePower
	for i := 0; i < MaxProxyRecursionDepth; i++ {
		delta[i+1] = a.ProxiedVSFVotes[i]
	}
	adjustProxiedWitnessVotesArray(s, a, delta, 0)
}

// SumProxiedVotes returns the total of an account's proxied_vsf_votes
// slots — useful for diagnostics and the C++ proxied_vsf_votes_total()
// helper.
func SumProxiedVotes(a *Account) int64 {
	var total int64
	for _, v := range a.ProxiedVSFVotes {
		total += v
	}
	return total
}

// _ silences unused-import warnings on builds without auth references.
var _ = auth.AccountName("")
