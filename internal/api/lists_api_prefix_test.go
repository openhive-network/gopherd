package api

import (
	"encoding/json"
	"fmt"
	"testing"

	"gopherd/internal/chain"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

// TestAccountPrefixScans pins the correctness of rangeTableByAccount.
//
// These handlers used to scan the whole table and filter in Go; they now seek
// straight to the account's key prefix. The scan is only exact because
// AppendAccountName writes a FIXED-WIDTH zero-padded field -- if that ever
// became variable-length, "alice" would start matching "alicebob" and this
// test is what catches it. Every case below therefore includes a sibling
// account whose name is a strict string prefix of the account under test.
func TestAccountPrefixScans(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}

	n := func(s string) auth.AccountName { return auth.AccountName(s) }
	sess := state.NewSession(db)

	// "alice" is the account under test; "alicebob" is the adversarial
	// neighbour that a variable-length key would let bleed in.
	objects.PutVestingDelegation(sess, &objects.VestingDelegation{Delegator: n("alice"), Delegatee: n("carol"), VestingShares: 11})
	objects.PutVestingDelegation(sess, &objects.VestingDelegation{Delegator: n("alice"), Delegatee: n("dave"), VestingShares: 22})
	objects.PutVestingDelegation(sess, &objects.VestingDelegation{Delegator: n("alicebob"), Delegatee: n("carol"), VestingShares: 99})

	objects.PutSavingsWithdraw(sess, &objects.SavingsWithdraw{From: n("alice"), RequestID: 1, To: n("carol"), Amount: 5})
	objects.PutSavingsWithdraw(sess, &objects.SavingsWithdraw{From: n("alicebob"), RequestID: 2, To: n("alice"), Amount: 7})

	objects.PutRCDirectDelegation(sess, &objects.RCDirectDelegation{From: n("alice"), To: n("carol"), DelegatedRC: 100})
	objects.PutRCDirectDelegation(sess, &objects.RCDirectDelegation{From: n("alice"), To: n("dave"), DelegatedRC: 200})
	objects.PutRCDirectDelegation(sess, &objects.RCDirectDelegation{From: n("alicebob"), To: n("carol"), DelegatedRC: 900})

	if err := sess.Commit(false); err != nil {
		t.Fatal(err)
	}
	srv := New(chain.New(db))

	rows := func(t *testing.T, method, params string) []interface{} {
		t.Helper()
		resp := call(t, srv, method, params)
		if resp.Error != nil {
			t.Fatalf("%s: %v", method, resp.Error)
		}
		var out []interface{}
		b, _ := json.Marshal(resp.Result)
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("%s: result not a list: %s", method, b)
		}
		return out
	}

	// Delegations: exactly alice's two, never alicebob's.
	got := rows(t, "condenser_api.get_vesting_delegations", `["alice","",100]`)
	if len(got) != 2 {
		t.Errorf("vesting_delegations: got %d rows, want 2 (alicebob must not leak): %v", len(got), got)
	}
	for _, r := range got {
		if d := r.(map[string]interface{})["delegator"]; d != "alice" {
			t.Errorf("vesting_delegations: foreign delegator %v", d)
		}
	}

	// by-FROM uses the prefix scan.
	if got := rows(t, "condenser_api.get_savings_withdraw_from", `["alice"]`); len(got) != 1 {
		t.Errorf("savings_withdraw_from alice: got %d rows, want 1: %v", len(got), got)
	}

	// by-TO cannot use it -- the recipient is not the key prefix. This is the
	// case that regressed when the prefix scan was applied to both directions:
	// alice's incoming withdrawal is stored under key "alicebob".
	if got := rows(t, "condenser_api.get_savings_withdraw_to", `["alice"]`); len(got) != 1 {
		t.Errorf("savings_withdraw_to alice: got %d rows, want 1 -- by-to must not use the from-prefix: %v", len(got), got)
	}

	// RC direct delegations: alice's two, not alicebob's. This one wraps its
	// list in an envelope object rather than returning a bare array.
	resp := call(t, srv, "rc_api.list_rc_direct_delegations", `{"start":["alice",""],"limit":100}`)
	if resp.Error != nil {
		t.Fatalf("list_rc_direct_delegations: %v", resp.Error)
	}
	var env struct {
		Delegations []struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"rc_direct_delegations"`
	}
	b, _ := json.Marshal(resp.Result)
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("list_rc_direct_delegations: %v (%s)", err, b)
	}
	if len(env.Delegations) != 2 {
		t.Errorf("list_rc_direct_delegations: got %d rows, want 2: %s", len(env.Delegations), b)
	}
	for _, d := range env.Delegations {
		if d.From != "alice" {
			t.Errorf("list_rc_direct_delegations: foreign delegator %q", d.From)
		}
	}
}

// BenchmarkAccountLookup quantifies why the prefix scan matters: it contrasts
// the old whole-table walk against the keyed seek on a table holding many
// accounts' rows. The old form's cost grows with TOTAL table size, which is
// what made these handlers a remote DoS lever -- an attacker paid for one cheap
// RPC and made the node walk every row while holding the read lock.
func BenchmarkAccountLookup(b *testing.B) {
	db, err := state.Open(b.TempDir(), 0)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		b.Fatal(err)
	}
	sess := state.NewSession(db)
	const accounts, perAccount = 20000, 5
	for i := 0; i < accounts; i++ {
		from := auth.AccountName(fmt.Sprintf("acct%06d", i))
		for j := 0; j < perAccount; j++ {
			objects.PutVestingDelegation(sess, &objects.VestingDelegation{
				Delegator:     from,
				Delegatee:     auth.AccountName(fmt.Sprintf("dst%06d", j)),
				VestingShares: int64(i),
			})
		}
	}
	if err := sess.Commit(false); err != nil {
		b.Fatal(err)
	}
	target := "acct010000"

	b.Run("prefix", func(b *testing.B) {
		_ = db // keep the session fresh per iteration
		for i := 0; i < b.N; i++ {
			n := 0
			s := state.NewSession(db)
			rangeTableByAccount(s, objects.VestingDelegationTable, target, func([]byte) bool { n++; return true })
			if n != perAccount {
				b.Fatalf("prefix scan saw %d rows, want %d", n, perAccount)
			}
		}
	})

	b.Run("fulltable", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			n := 0
			s := state.NewSession(db)
			rangeTable(s, objects.VestingDelegationTable, func(value []byte) bool {
				if vd, err := objects.ReadVestingDelegation(value); err == nil && string(vd.Delegator) == target {
					n++
				}
				return true
			})
			if n != perAccount {
				b.Fatalf("full scan saw %d rows, want %d", n, perAccount)
			}
		}
	})
}
