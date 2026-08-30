package evaluator

import (
	"errors"
	"strings"
	"testing"

	"gopherd/internal/crypto"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// hf29Set is the only way to observe post-HF29 behaviour: HF29 has no
// activation point, so SetForBlock can never produce a Set containing it (see
// TestUnscheduledHardforksNeverActivate). Built by adding the bit to the
// current set rather than in isolation, so the fixture models HF29 landing on
// top of a real chain — several of these operations require earlier forks
// (recurrent_transfer needs HF25, account_update2 needs HF21) and a bare
// HF0|HF29 set would fail them for the wrong reason.
func hf29Set() hardfork.Set {
	return preHF29Set() | hardfork.Set(1<<hardfork.HF29)
}

// preHF29Set is every scheduled hardfork through HF28 and NOT HF29 — what a
// node at today's head actually runs under.
func preHF29Set() hardfork.Set {
	return hardfork.SetForBlock(hardfork.MainnetActivationBlock[hardfork.HF28] + 1)
}

// openAuthority has weight_threshold == 0. sign_state treats such a threshold
// as already satisfied, so ANY signature passes it.
func openAuthority() auth.Authority {
	return auth.Authority{WeightThreshold: 0}
}

func closedAuthority() auth.Authority {
	var k crypto.PublicKey
	k[0] = 0x02
	k[1] = 0x11
	return auth.Authority{
		WeightThreshold: 1,
		KeyAuths:        []auth.KeyAuth{{Key: k, Weight: 1}},
	}
}

func seedAccountWithAuth(t *testing.T, name string) *state.Session {
	t.Helper()
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := state.NewSession(db)
	objects.PutAccount(s, &objects.Account{Name: auth.AccountName(name)})
	objects.PutAccountAuthority(s, &objects.AccountAuthority{
		Account: auth.AccountName(name),
		Owner:   closedAuthority(),
		Active:  closedAuthority(),
		Posting: closedAuthority(),
	})
	if err := s.Commit(false); err != nil {
		t.Fatal(err)
	}
	return state.NewSession(db)
}

// TestHF29OpenAuthorityGate covers FIX_OPEN_AUTHORITY across BOTH halves of
// hived's `is_in_control() || has_hardfork()` gate, for account_update and
// account_update2.
//
// The replay case is the one worth stating plainly: an open authority must
// still be ACCEPTED when not in control and before the fork. Mainnet contains
// transactions that set one (that is what issue #586 is about), so a node that
// rejected them could not replay the chain.
func TestHF29OpenAuthorityGate(t *testing.T) {
	roles := []struct {
		name  string
		build func(a auth.Authority) op.AccountUpdate
	}{
		{"owner", func(a auth.Authority) op.AccountUpdate {
			return op.AccountUpdate{Account: "alice", Owner: &a}
		}},
		{"active", func(a auth.Authority) op.AccountUpdate {
			return op.AccountUpdate{Account: "alice", Active: &a}
		}},
		{"posting", func(a auth.Authority) op.AccountUpdate {
			return op.AccountUpdate{Account: "alice", Posting: &a}
		}},
	}

	cases := []struct {
		name       string
		inControl  bool
		hardforks  hardfork.Set
		wantReject bool
	}{
		{"admission before the fork (is_in_control half)", true, preHF29Set(), true},
		{"after the fork while replaying (has_hardfork half)", false, hf29Set(), true},
		{"replaying history before the fork", false, preHF29Set(), false},
	}

	for _, tc := range cases {
		for _, role := range roles {
			t.Run(tc.name+"/"+role.name, func(t *testing.T) {
				s := seedAccountWithAuth(t, "alice")
				ctx := &Context{DB: s, InControl: tc.inControl, Hardforks: tc.hardforks, HeadBlockTime: 100_000}
				err := Apply(ctx, role.build(openAuthority()))
				if tc.wantReject {
					if !errors.Is(err, ErrOpenAuthority) {
						t.Fatalf("open %s authority: got %v, want ErrOpenAuthority", role.name, err)
					}
					return
				}
				if errors.Is(err, ErrOpenAuthority) {
					t.Fatalf("open %s authority rejected while replaying pre-HF29 history; mainnet contains such transactions and they must still apply", role.name)
				}
			})
		}
	}

	// A normal authority is unaffected by the rule in every configuration —
	// otherwise the assertions above would pass for the wrong reason.
	t.Run("closed authority always accepted", func(t *testing.T) {
		for _, tc := range cases {
			s := seedAccountWithAuth(t, "alice")
			ctx := &Context{DB: s, InControl: tc.inControl, Hardforks: tc.hardforks, HeadBlockTime: 100_000}
			a := closedAuthority()
			if err := Apply(ctx, op.AccountUpdate{Account: "alice", Owner: &a}); err != nil {
				t.Fatalf("%s: closed authority rejected: %v", tc.name, err)
			}
		}
	})

	t.Run("account_update2", func(t *testing.T) {
		// account_update2 is HF21+, so its Hardforks set must carry HF21 too.
		s := seedAccountWithAuth(t, "alice")
		a := openAuthority()
		ctx := &Context{DB: s, InControl: true, Hardforks: preHF29Set(), HeadBlockTime: 100_000}
		if err := Apply(ctx, op.AccountUpdate2{Account: "alice", Owner: &a}); !errors.Is(err, ErrOpenAuthority) {
			t.Fatalf("account_update2 open owner: got %v, want ErrOpenAuthority", err)
		}
		s2 := seedAccountWithAuth(t, "alice")
		ctx2 := &Context{DB: s2, InControl: false, Hardforks: preHF29Set(), HeadBlockTime: 100_000}
		if err := Apply(ctx2, op.AccountUpdate2{Account: "alice", Owner: &a}); errors.Is(err, ErrOpenAuthority) {
			t.Fatal("account_update2 open owner rejected while replaying pre-HF29 history")
		}
	})
}

// TestHF29RecoveryAuthoritySizeGate covers
// FIX_ENFORCEMENT_OF_AUTHORITY_SIZE_ON_RECOVERY_REQUEST.
func TestHF29RecoveryAuthoritySizeGate(t *testing.T) {
	oversized := auth.Authority{WeightThreshold: 1}
	for i := 0; i < maxAuthorityMembership+1; i++ {
		var k crypto.PublicKey
		k[0] = 0x02
		k[1] = byte(i)
		k[2] = byte(i >> 8)
		oversized.KeyAuths = append(oversized.KeyAuths, auth.KeyAuth{Key: k, Weight: 1})
	}
	if len(oversized.AccountAuths)+len(oversized.KeyAuths) != maxAuthorityMembership+1 {
		t.Fatalf("fixture: built %d members, want %d", len(oversized.KeyAuths), maxAuthorityMembership+1)
	}

	seed := func(t *testing.T) *state.Session {
		t.Helper()
		db, err := state.Open(t.TempDir(), 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		s := state.NewSession(db)
		objects.PutAccount(s, &objects.Account{Name: "victim", RecoveryAccount: "helper"})
		objects.PutAccount(s, &objects.Account{Name: "helper"})
		if err := s.Commit(false); err != nil {
			t.Fatal(err)
		}
		return state.NewSession(db)
	}

	req := op.RequestAccountRecovery{
		RecoveryAccount:   "helper",
		AccountToRecover:  "victim",
		NewOwnerAuthority: oversized,
	}

	t.Run("rejected at admission", func(t *testing.T) {
		ctx := &Context{DB: seed(t), InControl: true, Hardforks: preHF29Set(), HeadBlockTime: 100_000}
		if err := Apply(ctx, req); !errors.Is(err, ErrAuthoritySize) {
			t.Fatalf("got %v, want ErrAuthoritySize", err)
		}
	})
	t.Run("rejected after the fork", func(t *testing.T) {
		ctx := &Context{DB: seed(t), InControl: false, Hardforks: hf29Set(), HeadBlockTime: 100_000}
		if err := Apply(ctx, req); !errors.Is(err, ErrAuthoritySize) {
			t.Fatalf("got %v, want ErrAuthoritySize", err)
		}
	})
	t.Run("accepted while replaying pre-HF29", func(t *testing.T) {
		ctx := &Context{DB: seed(t), InControl: false, Hardforks: preHF29Set(), HeadBlockTime: 100_000}
		if err := Apply(ctx, req); errors.Is(err, ErrAuthoritySize) {
			t.Fatal("oversized recovery authority rejected during pre-HF29 replay")
		}
	})
	t.Run("exactly at the limit is allowed", func(t *testing.T) {
		atLimit := oversized
		atLimit.KeyAuths = oversized.KeyAuths[:maxAuthorityMembership]
		ctx := &Context{DB: seed(t), InControl: true, Hardforks: preHF29Set(), HeadBlockTime: 100_000}
		ok := req
		ok.NewOwnerAuthority = atLimit
		if err := Apply(ctx, ok); errors.Is(err, ErrAuthoritySize) {
			t.Fatalf("authority with exactly %d members rejected; the limit is inclusive", maxAuthorityMembership)
		}
	})
}

// TestValidateAuthSizeCountsBothMaps pins the detail that makes the limit a
// real limit: hived counts account_auths and key_auths TOGETHER
// (hive_operations.cpp:36). Counting them separately would let an attacker
// install 80 members while passing a per-map check of 40.
func TestValidateAuthSizeCountsBothMaps(t *testing.T) {
	a := auth.Authority{WeightThreshold: 1}
	for i := 0; i < 21; i++ {
		var k crypto.PublicKey
		k[0], k[1] = 0x02, byte(i)
		a.KeyAuths = append(a.KeyAuths, auth.KeyAuth{Key: k, Weight: 1})
		a.AccountAuths = append(a.AccountAuths, auth.AccountAuth{
			Account: auth.AccountName("acct" + string(rune('a'+i%26))), Weight: 1,
		})
	}
	// 21 + 21 = 42 > 40, though neither map alone exceeds the limit.
	if err := validateAuthSize(a, "x"); !errors.Is(err, ErrAuthoritySize) {
		t.Fatalf("got %v, want ErrAuthoritySize (21 keys + 21 accounts = 42 > %d)", err, maxAuthorityMembership)
	}
}

// TestHF29RecurrentTransferGateIsHardforkOnly is the asymmetry test, and the
// most important one in this file.
//
// ALLOW_MODIFY_LAST_RECURRENT_TRANSFER RELAXES a rule, so hived gates it on
// has_hardfork() ALONE — there is no is_in_control() half, unlike the other
// three HF29 changes. Wiring it through the shared hf29Rule helper would look
// natural and be wrong: we would start accepting and relaying transactions
// that every other node on the network still rejects, and any block we built
// containing one would be orphaned.
func TestHF29RecurrentTransferGateIsHardforkOnly(t *testing.T) {
	seed := func(t *testing.T) *state.Session {
		t.Helper()
		db, err := state.Open(t.TempDir(), 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		s := state.NewSession(db)
		objects.PutAccount(s, &objects.Account{Name: "alice", Balance: 1_000_000, OpenRecurrentTransfers: 1})
		objects.PutAccount(s, &objects.Account{Name: "bob"})
		objects.PutRecurrentTransfer(s, &objects.RecurrentTransfer{
			From: "alice", To: "bob", PairID: 0,
			Amount:     1_000,
			Symbol:     uint32(asset.HiveSymbol),
			Recurrence: 24, Executions: 3,
		})
		if err := s.Commit(false); err != nil {
			t.Fatal(err)
		}
		return state.NewSession(db)
	}

	// Modify the existing schedule down to its last execution.
	modify := op.RecurrentTransfer{
		From: "alice", To: "bob", Amount: hive(2_000),
		Recurrence: 24, Executions: 1, Memo: "final",
	}

	t.Run("refused at admission before the fork", func(t *testing.T) {
		// InControl is TRUE here. If this rule were wired through hf29Rule
		// like the tightening ones, the op would be ACCEPTED and this fails.
		ctx := &Context{DB: seed(t), InControl: true, Hardforks: preHF29Set(), HeadBlockTime: 100_000}
		err := Apply(ctx, modify)
		if err == nil || !strings.Contains(err.Error(), "at least 2") {
			t.Fatalf("got %v, want rejection: a LOOSENING rule must not take effect at admission before the fork activates", err)
		}
	})

	t.Run("allowed after the fork", func(t *testing.T) {
		s := seed(t)
		ctx := &Context{DB: s, InControl: false, Hardforks: hf29Set(), HeadBlockTime: 100_000}
		if err := Apply(ctx, modify); err != nil {
			t.Fatalf("post-HF29 modify to executions=1: %v", err)
		}
		rt, err := objects.GetRecurrentTransfer(s, "alice", "bob", 0)
		if err != nil {
			t.Fatalf("schedule missing after modify: %v", err)
		}
		if rt.Executions != 1 {
			t.Errorf("executions = %d, want 1", rt.Executions)
		}
		if rt.Amount != 2_000 {
			t.Errorf("amount = %d, want 2000 (the point of #786 is changing the final payment)", rt.Amount)
		}
	})

	t.Run("create still requires two executions after the fork", func(t *testing.T) {
		// HF29 relaxes MODIFY only. A fresh 1-execution transfer would fire
		// immediately and delete itself, so the create floor stays.
		s := seed(t)
		ctx := &Context{DB: s, InControl: false, Hardforks: hf29Set(), HeadBlockTime: 100_000}
		err := Apply(ctx, op.RecurrentTransfer{
			From: "alice", To: "carol", Amount: hive(1_000),
			Recurrence: 24, Executions: 1,
		})
		if err == nil {
			t.Fatal("post-HF29 CREATE with executions=1 was accepted; HF29 relaxes the modify path only")
		}
	})
}
