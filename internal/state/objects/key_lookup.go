package objects

import (
	"gopherd/internal/crypto"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

// Key-lookup index: public key -> accounts whose authorities grant it.
//
// This backs account_by_key_api.get_key_references, which is how wallets ask
// "which accounts does this key control?". Without an index that handler has to
// walk EVERY account and load EVERY authority row -- ~2M accounts on mainnet --
// while holding the apply read lock, so one cheap RPC stalls block application.
//
// Contents match hived's account_by_key plugin exactly
// (account_by_key_plugin.cpp update_key_lookup): the deduplicated union of the
// owner, active and posting key_auths. The MEMO key is deliberately NOT
// indexed -- hived never puts it in key_lookup, so matching it would return
// accounts hived does not.
const AccountAuthorityByKeyIndex state.IndexID = 1

// keyLookupKey builds (pubkey, account_name). AppendAccountName is fixed-width,
// so the 33-byte public key is an exact prefix and one key's rows can never
// bleed into another's.
func keyLookupKey(key crypto.PublicKey, account auth.AccountName) []byte {
	out := make([]byte, 0, len(key)+state.AccountNameKeyLen)
	out = append(out, key[:]...)
	return state.AppendAccountName(out, string(account))
}

// authorityKeySet mirrors hived's flat_set: the DEDUPLICATED union of the three
// authorities' keys. Dedup is load-bearing -- the same key commonly appears in
// more than one authority and yields the identical index row, so treating them
// as separate entries would let removing one authority drop a row another
// authority still needs.
func authorityKeySet(a *AccountAuthority) map[crypto.PublicKey]struct{} {
	set := make(map[crypto.PublicKey]struct{}, 4)
	for _, au := range []auth.Authority{a.Owner, a.Active, a.Posting} {
		for _, ka := range au.KeyAuths {
			if !ka.Key.IsZero() {
				set[ka.Key] = struct{}{}
			}
		}
	}
	return set
}

// reindexKeys applies the difference between two key sets for one account.
// Only genuine additions and removals touch the store, so re-storing an
// unchanged authority costs no writes.
func reindexKeys(s *state.Session, account auth.AccountName, old, new map[crypto.PublicKey]struct{}) {
	for k := range old {
		if _, still := new[k]; !still {
			s.Delete(AccountAuthorityTable, AccountAuthorityByKeyIndex, keyLookupKey(k, account))
		}
	}
	for k := range new {
		if _, had := old[k]; !had {
			s.Put(AccountAuthorityTable, AccountAuthorityByKeyIndex, keyLookupKey(k, account), nil)
		}
	}
}

// RangeAccountsByKey visits the accounts whose authorities reference key,
// stopping early if fn returns false.
func RangeAccountsByKey(s *state.Session, key crypto.PublicKey, fn func(account string) bool) {
	_ = s.Range(AccountAuthorityTable, AccountAuthorityByKeyIndex, key[:], func(k, _ []byte) (bool, error) {
		return fn(state.DecodeAccountName(k[len(key):])), nil
	})
}

// RebuildKeyLookupIndex recomputes the index from scratch, returning the number
// of accounts indexed.
//
// Required because the index was introduced after mainnet state already
// existed: an account whose authority was last written before the index shipped
// has no rows, and a partially populated index yields WRONG (silently
// incomplete) answers rather than merely slow ones. Run once at startup when
// the index is absent.
func RebuildKeyLookupIndex(s *state.Session) (int, error) {
	n := 0
	err := s.Range(AccountAuthorityTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
		a, err := ReadAccountAuthority(value)
		if err != nil {
			return false, err
		}
		reindexKeys(s, a.Account, nil, authorityKeySet(&a))
		n++
		return true, nil
	})
	return n, err
}

// HasKeyLookupIndex reports whether the index holds any rows, so startup can
// tell "not built yet" from "built and this key simply has no accounts".
func HasKeyLookupIndex(s *state.Session) bool {
	found := false
	_ = s.Range(AccountAuthorityTable, AccountAuthorityByKeyIndex, nil, func([]byte, []byte) (bool, error) {
		found = true
		return false, nil
	})
	return found
}
