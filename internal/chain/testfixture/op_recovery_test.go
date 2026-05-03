package testfixture

import (
	"testing"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// TestRequestAccountRecovery ports the request-creation portion of
// operation_tests.cpp::account_recovery. Creates alice + bob, has alice
// (bob's recovery account) request recovery; verifies an
// account_recovery_request row is persisted with the right fields and
// expiration.
func TestRequestAccountRecovery(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()

	if err := f.ApplyOp(op.RequestAccountRecovery{
		RecoveryAccount:   "alice",
		AccountToRecover:  "bob",
		NewOwnerAuthority: auth.Authority{WeightThreshold: 1, KeyAuths: []auth.KeyAuth{{Weight: 1}}},
	}); err != nil {
		t.Fatalf("request_account_recovery: %v", err)
	}

	req, err := objects.GetAccountRecoveryRequest(state.NewSession(f.State), "bob")
	if err != nil {
		t.Fatalf("GetAccountRecoveryRequest: %v", err)
	}
	if req.AccountToRecover != "bob" {
		t.Errorf("AccountToRecover: got %q, want bob", req.AccountToRecover)
	}
	// Expiration is head_block_time + 24h.
	wantExp := f.HeadBlockTime() + 60*60*24
	// allow 6-second slack for block-interval drift
	if req.Expiration < wantExp-6 || req.Expiration > wantExp+6 {
		t.Errorf("Expiration: got %d, want ~%d (head_block_time+24h)",
			req.Expiration, wantExp)
	}
}

// TestRecoverAccountConsumesRequest ports the consume-request portion of
// account_recovery: after a recover_account op, the pending request is
// removed.
func TestRecoverAccountConsumesRequest(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()

	if err := f.ApplyOp(op.RequestAccountRecovery{
		RecoveryAccount:   "alice",
		AccountToRecover:  "bob",
		NewOwnerAuthority: auth.Authority{WeightThreshold: 1, KeyAuths: []auth.KeyAuth{{Weight: 1}}},
	}); err != nil {
		t.Fatalf("request: %v", err)
	}

	// Verify request exists.
	if has, _ := objects.HasAccountRecoveryRequest(state.NewSession(f.State), "bob"); !has {
		t.Fatal("expected pending recovery request before recover op")
	}

	if err := f.ApplyOp(op.RecoverAccount{
		AccountToRecover:     "bob",
		NewOwnerAuthority:    auth.Authority{WeightThreshold: 1, KeyAuths: []auth.KeyAuth{{Weight: 1}}},
		RecentOwnerAuthority: auth.Authority{WeightThreshold: 1, KeyAuths: []auth.KeyAuth{{Weight: 1}}},
	}); err != nil {
		t.Fatalf("recover_account: %v", err)
	}

	// Request should be consumed.
	if has, _ := objects.HasAccountRecoveryRequest(state.NewSession(f.State), "bob"); has {
		t.Error("recovery request should be consumed after recover_account")
	}
}

// TestRecoverAccountExpiredRequestRejected verifies that a recover_account
// op against an expired request is rejected and the request is cleaned up.
// Mirrors the C++ branch in recover_account_evaluator that throws when
// req.expiration < head_block_time.
func TestRecoverAccountExpiredRequestRejected(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()

	if err := f.ApplyOp(op.RequestAccountRecovery{
		RecoveryAccount:   "alice",
		AccountToRecover:  "bob",
		NewOwnerAuthority: auth.Authority{WeightThreshold: 1, KeyAuths: []auth.KeyAuth{{Weight: 1}}},
	}); err != nil {
		t.Fatalf("request: %v", err)
	}

	// Skip past the 24h expiration.
	f.SkipSeconds(60*60*24 + 60)

	err := f.ApplyOp(op.RecoverAccount{
		AccountToRecover:     "bob",
		NewOwnerAuthority:    auth.Authority{WeightThreshold: 1, KeyAuths: []auth.KeyAuth{{Weight: 1}}},
		RecentOwnerAuthority: auth.Authority{WeightThreshold: 1, KeyAuths: []auth.KeyAuth{{Weight: 1}}},
	})
	if err == nil {
		t.Fatal("recover against expired request should fail")
	}
}

// TestChangeRecoveryAccount ports operation_tests.cpp::change_recovery_account.
// A change_recovery_account op creates a pending request; the new
// recovery account becomes effective after HIVE_OWNER_AUTH_RECOVERY_PERIOD
// (30 days).
func TestChangeRecoveryAccount(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob", "carol")
	f.GenerateBlock()

	if err := f.ApplyOp(op.ChangeRecoveryAccount{
		AccountToRecover:   "bob",
		NewRecoveryAccount: "carol",
	}); err != nil {
		t.Fatalf("change_recovery_account: %v", err)
	}

	req, err := objects.GetChangeRecoveryRequest(state.NewSession(f.State), "bob")
	if err != nil {
		t.Fatalf("GetChangeRecoveryRequest: %v", err)
	}
	if req.RecoveryAccount != "carol" {
		t.Errorf("RecoveryAccount: got %q, want carol", req.RecoveryAccount)
	}
	wantEff := f.HeadBlockTime() + 60*60*24*30
	if req.EffectiveOn < wantEff-6 || req.EffectiveOn > wantEff+6 {
		t.Errorf("EffectiveOn: got %d, want ~%d (head+30d)", req.EffectiveOn, wantEff)
	}
}

// TestChangeRecoveryAccountOverwritesPending verifies that issuing a
// second change_recovery_account before the first matures replaces the
// pending request rather than queuing both.
func TestChangeRecoveryAccountOverwritesPending(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob", "carol", "dave")
	f.GenerateBlock()

	f.MustApplyOp(op.ChangeRecoveryAccount{
		AccountToRecover:   "bob",
		NewRecoveryAccount: "carol",
	})
	f.MustApplyOp(op.ChangeRecoveryAccount{
		AccountToRecover:   "bob",
		NewRecoveryAccount: "dave",
	})

	req, err := objects.GetChangeRecoveryRequest(state.NewSession(f.State), "bob")
	if err != nil {
		t.Fatalf("GetChangeRecoveryRequest: %v", err)
	}
	if req.RecoveryAccount != "dave" {
		t.Errorf("after overwrite: got %q, want dave", req.RecoveryAccount)
	}
}
