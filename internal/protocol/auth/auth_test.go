package auth

import (
	"bytes"
	"testing"

	"gopherd/internal/crypto"
	"gopherd/internal/fcraw"
)

func TestEmptyAuthorityRoundTrip(t *testing.T) {
	a := Authority{WeightThreshold: 1}
	wire := AppendAuthority(nil, a)
	got, err := ReadAuthority(fcraw.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	if got.WeightThreshold != 1 {
		t.Errorf("threshold: got %d, want 1", got.WeightThreshold)
	}
	if len(got.AccountAuths) != 0 || len(got.KeyAuths) != 0 {
		t.Errorf("expected empty auths, got %+v", got)
	}
	// Bytes layout: 4-byte threshold + 1-byte 0 (account_auths count) + 1-byte 0 (key_auths count).
	if len(wire) != 6 {
		t.Errorf("empty authority wire size = %d, want 6", len(wire))
	}
}

func TestAuthorityRoundTripWithAccountAndKeyAuths(t *testing.T) {
	mustKey := func(s string) PublicKey {
		k, err := crypto.DecodePublicKey(s)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	// Use the mainnet HIVE_INIT_PUBLIC_KEY twice (different "owners").
	const initKey = "STM8GC13uCZbP44HzMLV6zPZGwVQ8Nt4Kji8PapsPiNq1BK153XTX"
	k1 := mustKey(initKey)
	// Make a second distinct key by tweaking byte 0.
	k2 := k1
	k2[0] ^= 0x01

	a := Authority{
		WeightThreshold: 2,
		AccountAuths: []AccountAuth{
			{Account: "alice", Weight: 1},
			{Account: "bob", Weight: 1},
		},
		KeyAuths: []KeyAuth{
			{Key: k1, Weight: 1},
			{Key: k2, Weight: 2},
		},
	}
	wire := AppendAuthority(nil, a)
	got, err := ReadAuthority(fcraw.NewReader(wire))
	if err != nil {
		t.Fatalf("ReadAuthority: %v", err)
	}
	if got.WeightThreshold != a.WeightThreshold {
		t.Errorf("threshold mismatch")
	}
	if len(got.AccountAuths) != 2 {
		t.Fatalf("account_auths count: %d", len(got.AccountAuths))
	}
	if got.AccountAuths[0].Account != "alice" || got.AccountAuths[1].Account != "bob" {
		t.Errorf("account auths: %+v", got.AccountAuths)
	}
	if len(got.KeyAuths) != 2 {
		t.Fatalf("key_auths count: %d", len(got.KeyAuths))
	}
	if got.KeyAuths[0].Key != k1 || got.KeyAuths[1].Key != k2 {
		t.Errorf("key auths order/value")
	}

	// Re-marshal and check bit-exact equivalence.
	wire2 := AppendAuthority(nil, got)
	if !bytes.Equal(wire, wire2) {
		t.Fatalf("re-marshal differs:\n got %x\nwant %x", wire2, wire)
	}
}

func TestIsSorted(t *testing.T) {
	good := Authority{
		AccountAuths: []AccountAuth{{Account: "alice", Weight: 1}, {Account: "bob", Weight: 1}},
	}
	if !good.IsSorted() {
		t.Error("alice<bob should be sorted")
	}
	bad := Authority{
		AccountAuths: []AccountAuth{{Account: "bob", Weight: 1}, {Account: "alice", Weight: 1}},
	}
	if bad.IsSorted() {
		t.Error("bob>alice should not be sorted")
	}
	dup := Authority{
		AccountAuths: []AccountAuth{{Account: "alice", Weight: 1}, {Account: "alice", Weight: 2}},
	}
	if dup.IsSorted() {
		t.Error("duplicate keys should not be flagged as sorted (must be strictly increasing)")
	}
}
