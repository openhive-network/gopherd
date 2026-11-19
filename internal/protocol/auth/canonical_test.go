package auth

import (
	"bytes"
	"testing"

	"gopherd/internal/fcraw"
)

func key(b byte) PublicKey {
	var k PublicKey
	k[0] = 0x02
	k[32] = b
	return k
}

// TestReadAuthorityInsertsLikeFC pins fc's flat_map unpack: entries come out
// sorted by key and a repeated key keeps its first weight. A key listed twice
// must count once when signatures are weighed, as it does in hived.
func TestReadAuthorityInsertsLikeFC(t *testing.T) {
	wire := Authority{
		WeightThreshold: 2,
		AccountAuths: []AccountAuth{
			{Account: "carol", Weight: 1},
			{Account: "alice", Weight: 5},
			{Account: "carol", Weight: 9},
		},
		KeyAuths: []KeyAuth{
			{Key: key(7), Weight: 1},
			{Key: key(3), Weight: 4},
			{Key: key(7), Weight: 1},
		},
	}
	got, err := ReadAuthority(fcraw.NewReader(AppendAuthority(nil, wire)))
	if err != nil {
		t.Fatal(err)
	}
	wantAccounts := []AccountAuth{{Account: "alice", Weight: 5}, {Account: "carol", Weight: 1}}
	wantKeys := []KeyAuth{{Key: key(3), Weight: 4}, {Key: key(7), Weight: 1}}
	if len(got.AccountAuths) != 2 || got.AccountAuths[0] != wantAccounts[0] || got.AccountAuths[1] != wantAccounts[1] {
		t.Errorf("account_auths %+v, want %+v", got.AccountAuths, wantAccounts)
	}
	if len(got.KeyAuths) != 2 || got.KeyAuths[0] != wantKeys[0] || got.KeyAuths[1] != wantKeys[1] {
		t.Errorf("key_auths %+v, want %+v", got.KeyAuths, wantKeys)
	}
	if !got.IsSorted() {
		t.Error("decoded authority is not sorted")
	}
}

// TestCanonicalKeepsCanonicalInput pins that an authority already in fc's
// form is returned unchanged and round-trips byte for byte.
func TestCanonicalKeepsCanonicalInput(t *testing.T) {
	a := Authority{
		WeightThreshold: 1,
		AccountAuths:    []AccountAuth{{Account: "a", Weight: 1}, {Account: "b", Weight: 1}},
		KeyAuths:        []KeyAuth{{Key: key(1), Weight: 1}, {Key: key(2), Weight: 1}},
	}
	enc := AppendAuthority(nil, a)
	got, err := ReadAuthority(fcraw.NewReader(enc))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(AppendAuthority(nil, got), enc) {
		t.Fatal("canonical authority does not round-trip")
	}
}

// FuzzReadAuthority decodes authorities from arbitrary bytes. It must not
// panic, the result must be in fc's canonical form, and canonical form must
// be stable under re-encoding.
func FuzzReadAuthority(f *testing.F) {
	f.Add(AppendAuthority(nil, Authority{WeightThreshold: 1, KeyAuths: []KeyAuth{{Key: key(1), Weight: 1}}}))
	f.Add(AppendAuthority(nil, Authority{WeightThreshold: 2, AccountAuths: []AccountAuth{{Account: "b", Weight: 1}, {Account: "a", Weight: 1}, {Account: "b", Weight: 3}}}))
	f.Fuzz(func(t *testing.T, src []byte) {
		a, err := ReadAuthority(fcraw.NewReader(src))
		if err != nil {
			return
		}
		if !a.IsSorted() {
			t.Fatalf("decoded authority not sorted and unique: %+v", a)
		}
		enc := AppendAuthority(nil, a)
		b, err := ReadAuthority(fcraw.NewReader(enc))
		if err != nil || !bytes.Equal(AppendAuthority(nil, b), enc) {
			t.Fatalf("canonical authority not stable (err=%v)", err)
		}
	})
}
