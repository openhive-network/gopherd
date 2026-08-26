package api

import (
	"encoding/json"
	"fmt"
	"testing"

	"gopherd/internal/chain"
	"gopherd/internal/crypto"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

func testKey(b byte) crypto.PublicKey {
	var k crypto.PublicKey
	k[0] = 0x02
	for i := 1; i < len(k); i++ {
		k[i] = b
	}
	return k
}

func keyAuth(k crypto.PublicKey) auth.Authority {
	return auth.Authority{WeightThreshold: 1, KeyAuths: []auth.KeyAuth{{Key: k, Weight: 1}}}
}

func keyRefs(t *testing.T, srv *Server, key crypto.PublicKey) []string {
	t.Helper()
	body, _ := json.Marshal([]interface{}{[]string{key.String()}})
	resp := call(t, srv, "condenser_api.get_key_references", string(body))
	if resp.Error != nil {
		t.Fatalf("get_key_references: %v", resp.Error)
	}
	var out [][]string
	b, _ := json.Marshal(resp.Result)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("get_key_references: %v (%s)", err, b)
	}
	if len(out) != 1 {
		t.Fatalf("get_key_references: got %d slots, want 1: %s", len(out), b)
	}
	return out[0]
}

// TestGetKeyReferences covers the public-key index end to end: lookup,
// incremental maintenance when a key is revoked, and hived's rule that the
// MEMO key is not a reference.
func TestGetKeyReferences(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}

	shared, aliceOnly, memoOnly := testKey(0x11), testKey(0x22), testKey(0x33)

	sess := state.NewSession(db)
	// alice: shared in owner, aliceOnly in active, and a memo key that is
	// deliberately NOT in any authority.
	objects.PutAccountAuthority(sess, &objects.AccountAuthority{
		Account: auth.AccountName("alice"),
		Owner:   keyAuth(shared), Active: keyAuth(aliceOnly), Posting: keyAuth(shared),
	})
	objects.PutAccountAuthority(sess, &objects.AccountAuthority{
		Account: auth.AccountName("bob"),
		Owner:   keyAuth(shared), Active: keyAuth(shared), Posting: keyAuth(shared),
	})
	a := objects.Account{Name: auth.AccountName("alice"), MemoKey: memoOnly}
	objects.PutAccount(sess, &a)
	if err := sess.Commit(false); err != nil {
		t.Fatal(err)
	}
	srv := New(chain.New(db))

	// The shared key references both accounts; bob lists it in all three
	// authorities and must still appear exactly ONCE (dedup).
	if got := keyRefs(t, srv, shared); len(got) != 2 {
		t.Errorf("shared key: got %v, want alice and bob exactly once each", got)
	}
	if got := keyRefs(t, srv, aliceOnly); len(got) != 1 || got[0] != "alice" {
		t.Errorf("aliceOnly key: got %v, want [alice]", got)
	}
	// hived's key_lookup indexes owner/active/posting only -- never the memo
	// key -- so a memo-only key references nothing.
	if got := keyRefs(t, srv, memoOnly); len(got) != 0 {
		t.Errorf("memo-only key: got %v, want [] (hived does not index memo keys)", got)
	}

	// Revoking aliceOnly must retire its index row...
	sess2 := state.NewSession(db)
	objects.PutAccountAuthority(sess2, &objects.AccountAuthority{
		Account: auth.AccountName("alice"),
		Owner:   keyAuth(shared), Active: keyAuth(shared), Posting: keyAuth(shared),
	})
	if err := sess2.Commit(false); err != nil {
		t.Fatal(err)
	}
	if got := keyRefs(t, srv, aliceOnly); len(got) != 0 {
		t.Errorf("after revoke: aliceOnly still references %v, want []", got)
	}
	// ...without disturbing the key alice still shares with bob.
	if got := keyRefs(t, srv, shared); len(got) != 2 {
		t.Errorf("after revoke: shared key got %v, want 2 accounts", got)
	}
}

// TestKeyLookupBackfill covers state that predates the index: the rows are
// missing, so the rebuild must reconstruct them. A partially built index
// answers WRONG rather than slow, which is why startup backfills.
func TestKeyLookupBackfill(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}
	key := testKey(0x44)

	sess := state.NewSession(db)
	objects.PutAccountAuthority(sess, &objects.AccountAuthority{
		Account: auth.AccountName("carol"),
		Owner:   keyAuth(key), Active: keyAuth(key), Posting: keyAuth(key),
	})
	if err := sess.Commit(false); err != nil {
		t.Fatal(err)
	}

	// Simulate pre-index state by dropping every index row.
	wipe := state.NewSession(db)
	var keys [][]byte
	_ = wipe.Range(objects.AccountAuthorityTable, objects.AccountAuthorityByKeyIndex, nil,
		func(k, _ []byte) (bool, error) {
			keys = append(keys, append([]byte(nil), k...))
			return true, nil
		})
	if len(keys) == 0 {
		t.Fatal("expected index rows to wipe")
	}
	for _, k := range keys {
		wipe.Delete(objects.AccountAuthorityTable, objects.AccountAuthorityByKeyIndex, k)
	}
	if err := wipe.Commit(false); err != nil {
		t.Fatal(err)
	}

	c := chain.New(db)
	srv := New(c)
	if got := keyRefs(t, srv, key); len(got) != 0 {
		t.Fatalf("index was not actually wiped: %v", got)
	}
	if state.NewSession(db); objects.HasKeyLookupIndex(state.NewSession(db)) {
		t.Fatal("HasKeyLookupIndex true after wipe")
	}

	n, err := c.EnsureKeyLookupIndex()
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("EnsureKeyLookupIndex indexed 0 authorities")
	}
	if got := keyRefs(t, srv, key); len(got) != 1 || got[0] != "carol" {
		t.Errorf("after backfill: got %v, want [carol]", got)
	}

	// Second call must be a no-op, not a rebuild.
	if n2, err := c.EnsureKeyLookupIndex(); err != nil || n2 != 0 {
		t.Errorf("second EnsureKeyLookupIndex: n=%d err=%v, want 0/nil", n2, err)
	}
}

// BenchmarkKeyReferences contrasts the indexed lookup against the walk it
// replaced (every account + a per-account authority load). The old shape's cost
// grows with the ACCOUNT COUNT -- ~2M on mainnet -- which is what made one
// cheap RPC able to stall block application.
func BenchmarkKeyReferences(b *testing.B) {
	db, err := state.Open(b.TempDir(), 0)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		b.Fatal(err)
	}
	const accounts = 20000
	target := testKey(0xFF) // outside the generated 1..200 range, so exactly one account holds it
	sess := state.NewSession(db)
	for i := 0; i < accounts; i++ {
		name := auth.AccountName(fmt.Sprintf("acct%06d", i))
		k := testKey(byte(i%200 + 1))
		if i == accounts/2 {
			k = target
		}
		objects.PutAccountAuthority(sess, &objects.AccountAuthority{
			Account: name, Owner: keyAuth(k), Active: keyAuth(k), Posting: keyAuth(k),
		})
		a := objects.Account{Name: name}
		objects.PutAccount(sess, &a)
	}
	if err := sess.Commit(false); err != nil {
		b.Fatal(err)
	}

	b.Run("indexed", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			n := 0
			s := state.NewSession(db)
			objects.RangeAccountsByKey(s, target, func(string) bool { n++; return true })
			if n != 1 {
				b.Fatalf("indexed lookup found %d accounts, want 1", n)
			}
		}
	})

	b.Run("fullwalk", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			n := 0
			s := state.NewSession(db)
			_ = objects.RangeAccounts(s, func(a *objects.Account) (bool, error) {
				au, err := objects.GetAccountAuthority(s, a.Name)
				if err != nil {
					return true, nil
				}
				for _, aa := range []auth.Authority{au.Owner, au.Active, au.Posting} {
					for _, ka := range aa.KeyAuths {
						if ka.Key == target {
							n++
							return true, nil
						}
					}
				}
				return true, nil
			})
			if n != 1 {
				b.Fatalf("full walk found %d accounts, want 1", n)
			}
		}
	})
}
