package testfixture

// Port of hive_tes/tests/unit/plugin_tests/rc_plugin_tests.cpp into Go.
// One Go test function per BOOST_AUTO_TEST_CASE in the C++ source.
//
// Coverage map (11 C++ cases):
//
//   account_creation                — TestRCPluginAccountCreation (ported,
//                                      reduced: pre-HF13 PoW paths
//                                      stubbed; post-HF20 account_create
//                                      + create_claimed_account ported).
//   rc_usage_buckets                — TestRCPluginUsageBuckets (already
//                                      covered structurally by
//                                      TestRCUsageBucketRotation +
//                                      TestRCUsageBucketsAdvancedStub;
//                                      this is a documenting alias
//                                      pointing at those + skip on the
//                                      per-op cost flavor).
//   rc_single_recover_account       — TestRCPluginSingleRecoverAccount
//                                      (skipped — RC Phase 5).
//   rc_many_recover_accounts        — TestRCPluginManyRecoverAccounts
//                                      (skipped — RC Phase 5).
//   rc_multisig_recover_account     — TestRCPluginMultisigRecoverAccount
//                                      (skipped — RC Phase 5).
//   rc_tx_order_bug                 — TestRCPluginTxOrderBug
//                                      (skipped — RC Phase 5 + pop_block).
//   rc_pending_data_reset           — TestRCPluginPendingDataReset
//                                      (skipped — RC Phase 5; existing
//                                      TestRCPoolPendingDataReset covers
//                                      stats archive, NOT rc_pending_data
//                                      diff/usage which is a Phase 5
//                                      object).
//   rc_differential_usage_operations— TestRCPluginDifferentialUsageOperations
//                                      (skipped — RC Phase 5).
//   rc_differential_usage_negative  — TestRCPluginDifferentialUsageNegative
//                                      (skipped — RC Phase 5).
//   rc_differential_usage_many_ops  — TestRCPluginDifferentialUsageManyOps
//                                      (skipped — RC Phase 5).
//   rc_exception_during_modify      — TestRCPluginExceptionDuringModify
//                                      (skipped — RC Phase 5 not_enough_rc
//                                      exception path).
//
// Why so many skips: Phase 4 (resource_param + set_slot_delegator) and
// Phase 5 (per-tx RC cost deduction in the evaluator pipeline) are still
// deferred per the RC subsystem roadmap. Every C++ case here that
// exercises the actual rc-charging path (single/many/multisig recover,
// tx_order_bug, differential_usage_*, exception_during_modify) needs
// that machinery to fire. They're scaffolded as skips so the test names
// line up 1:1 with the C++ suite; the bodies become real assertions
// once Phase 5 lands.

import (
	"errors"
	"strings"
	"testing"

	"hived-go/internal/chain"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// TestRCPluginAccountCreation mirrors rc_plugin_tests.cpp::account_creation.
//
// The C++ test creates 5 actors at successive hardforks via every
// account-creation pathway (pow, pow2, account_create_with_delegation,
// account_create, create_claimed_account) and asserts an
// `rc_account_object` exists for each name after the block. Our port
// drops the rc_account_object indirection — RC fields live directly on
// objects.Account in this Go port — so the assertion becomes
// `f.HasAccount(name)` post-creation.
//
// Pre-HF13 PoW paths and the HF13-only pow2 path are skipped because
// the Go fixture deliberately bypasses the on-chain
// pow/pow2 path (it writes accounts via direct PutAccount, like
// hived's `account_create` debug helper). The post-HF17
// account_create_with_delegation, post-HF20 account_create, and
// post-HF24 create_claimed_account paths run through their real
// evaluators here.
func TestRCPluginAccountCreation(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)

	// initminer needs balance + vests for the fee/delegation.
	f.FundLiquid(InitMiner, 1_000_000)
	f.FundVesting(InitMiner, 1_000_000)
	f.GenerateBlock()

	pubKey := [33]byte{0x02, 'A'}

	// post-HF17 account_create_with_delegation path → "sam".
	if err := f.ApplyOp(op.AccountCreateWithDelegation{
		Fee:            HIVE(0),
		Delegation:     VESTS(100_000_000),
		Creator:        InitMiner,
		NewAccountName: "sam",
		Owner:          newAuth(pubKey),
		Active:         newAuth(pubKey),
		Posting:        newAuth(pubKey),
		MemoKey:        pubKey,
	}); err != nil {
		t.Fatalf("account_create_with_delegation(sam): %v", err)
	}
	if !f.HasAccount("sam") {
		t.Fatalf("expected account 'sam' to exist post-create_with_delegation")
	}

	// post-HF20 vanilla account_create path → "dave".
	f.HardforksUpTo(hardfork.HF20)
	if err := f.ApplyOp(op.AccountCreate{
		Fee:            HIVE(0),
		Creator:        InitMiner,
		NewAccountName: "dave",
		Owner:          newAuth(pubKey),
		Active:         newAuth(pubKey),
		Posting:        newAuth(pubKey),
		MemoKey:        pubKey,
	}); err != nil {
		t.Fatalf("account_create(dave): %v", err)
	}
	if !f.HasAccount("dave") {
		t.Fatalf("expected account 'dave' to exist post-account_create")
	}

	// post-HF20 create_claimed_account path → "greg". This requires
	// bumping initminer's pending_claimed_accounts first, exactly as
	// the C++ test does via debug_update.
	{
		// Bump pending_claimed_accounts on initminer (mirrors the
		// C++ debug_update step at line ~219 in account_creation).
		bumpPendingClaimedAccounts(t, f, InitMiner, 1)

		if err := f.ApplyOp(op.CreateClaimedAccount{
			Creator:        InitMiner,
			NewAccountName: "greg",
			Owner:          newAuth(pubKey),
			Active:         newAuth(pubKey),
			Posting:        newAuth(pubKey),
			MemoKey:        pubKey,
		}); err != nil {
			t.Fatalf("create_claimed_account(greg): %v", err)
		}
		if !f.HasAccount("greg") {
			t.Fatalf("expected account 'greg' to exist post-create_claimed_account")
		}
	}

	// The post-HF20 RC-init hook runs the manabar on each newly
	// created account; we already validate that path in
	// TestRCManabarPostHF20InitOnCreate. Here we just spot-check
	// that the recent creates left the manabar field set.
	dave := f.Account_("dave")
	if dave.RCManaLastUpdate == 0 {
		t.Fatalf("dave.RCManaLastUpdate should be initialized post-HF20")
	}
	greg := f.Account_("greg")
	if greg.RCManaLastUpdate == 0 {
		t.Fatalf("greg.RCManaLastUpdate should be initialized post-HF20")
	}
}

// bumpPendingClaimedAccounts adds n to the named account's
// PendingClaimedAccounts via direct state mutation. Mirrors the
// db_plugin->debug_update step in the C++ account_creation case for
// the create_claimed_account leg.
func bumpPendingClaimedAccounts(t *testing.T, f *Fixture, name string, n uint32) {
	t.Helper()
	s := state.NewSession(f.State)
	a, err := objects.GetAccount(s, name)
	if err != nil {
		t.Fatalf("bumpPendingClaimedAccounts: get %q: %v", name, err)
	}
	a.PendingClaimedAccounts += uint16(n)
	objects.PutAccount(s, &a)
	if err := s.Commit(false); err != nil {
		t.Fatalf("bumpPendingClaimedAccounts: commit: %v", err)
	}
}

// TestRCPluginUsageBuckets mirrors rc_plugin_tests.cpp::rc_usage_buckets.
//
// The C++ case exercises 24-hour rolling usage buckets across many
// transactions and asserts the per-resource consumption distributes
// into the right bucket as time advances. Phase 5 port: drive a few
// transfers post-HF20, generate a block, and assert the newest bucket's
// Used array picked up nonzero PendingUsage from the rc_pending_data
// flush hook.
func TestRCPluginUsageBuckets(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF26)
	f.Actors("alice", "bob")
	f.FundVesting("alice", 1_000_000)
	f.FundLiquid("alice", 100_000)
	f.GenerateBlock()

	// Snapshot the active bucket Used arrays BEFORE driving ops.
	beforeUsed := readActiveBucketUsed(t, f)

	// Drive 3 transfers. Each contributes history_bytes + market_bytes
	// + state_bytes (transaction_base) + execution_time to the active
	// bucket.
	for i := 0; i < 3; i++ {
		if err := f.ApplyOp(op.Transfer{
			From: "alice", To: "bob", Amount: HIVE(1),
			Memo: "rc",
		}); err != nil {
			t.Fatalf("transfer %d: %v", i, err)
		}
	}

	afterUsed := readActiveBucketUsed(t, f)
	// State bytes + execution time must have grown.
	if afterUsed[objects.RCStateBytes] <= beforeUsed[objects.RCStateBytes] {
		t.Fatalf("state_bytes: want growth, got before=%d after=%d",
			beforeUsed[objects.RCStateBytes], afterUsed[objects.RCStateBytes])
	}
	if afterUsed[objects.RCExecutionTime] <= beforeUsed[objects.RCExecutionTime] {
		t.Fatalf("execution_time: want growth, got before=%d after=%d",
			beforeUsed[objects.RCExecutionTime], afterUsed[objects.RCExecutionTime])
	}
	// history_bytes + market_bytes also grew (transfer is a market op).
	if afterUsed[objects.RCHistoryBytes] <= beforeUsed[objects.RCHistoryBytes] {
		t.Fatalf("history_bytes: want growth, got before=%d after=%d",
			beforeUsed[objects.RCHistoryBytes], afterUsed[objects.RCHistoryBytes])
	}
	if afterUsed[objects.RCMarketBytes] <= beforeUsed[objects.RCMarketBytes] {
		t.Fatalf("market_bytes (transfer is market op): want growth, got before=%d after=%d",
			beforeUsed[objects.RCMarketBytes], afterUsed[objects.RCMarketBytes])
	}
}

// readActiveBucketUsed returns the Used array of the newest
// rc_usage_bucket.
func readActiveBucketUsed(t *testing.T, f *Fixture) [objects.RCNumResources]int64 {
	t.Helper()
	s := state.NewSession(f.State)
	maxSeqno := uint32(0)
	var used [objects.RCNumResources]int64
	_ = s.Range(objects.RCUsageBucketTable, state.PrimaryIndex, nil,
		func(_, val []byte) (bool, error) {
			b, err := objects.ReadRCUsageBucket(val)
			if err != nil {
				return true, nil
			}
			if b.BucketSeqno > maxSeqno {
				maxSeqno = b.BucketSeqno
				used = b.Used
			}
			return true, nil
		})
	return used
}

// TestRCPluginSingleRecoverAccount mirrors rc_plugin_tests.cpp::
// rc_single_recover_account.
//
// The C++ test verifies a standalone recover_account (with single-auth
// new_owner_authority and 2 signatures) is RC-free. CountResources
// implements the subsidized-op rule; we assert directly on its output
// rather than going through the apply pipeline (since porting
// recover_account end-to-end requires the change_recovery_account
// + request_account_recovery setup the C++ test drives — that's
// 100+ LOC of fixture setup).
func TestRCPluginSingleRecoverAccount(t *testing.T) {
	// Build a synthetic signed_transaction with exactly one
	// recover_account op (single key auth) and 2 signatures.
	var pubKey [33]byte
	pubKey[0], pubKey[1] = 0x02, 'A'
	stx := newSubsidizedRecoverAccountTx("alice", pubKey, 2)
	var result [objects.RCNumResources]int64
	chain.CountResources(stx, int64(50), &result) // txSize ignored when subsidized
	for i := 0; i < objects.RCNumResources; i++ {
		if result[i] != 0 {
			t.Fatalf("subsidized tx should have zero RC usage, got [%d]=%d",
				i, result[i])
		}
	}
}

// TestRCPluginManyRecoverAccounts mirrors rc_plugin_tests.cpp::
// rc_many_recover_accounts.
//
// The C++ test packs 2 recover_account ops in one tx and observes the
// subsidy doesn't apply. CountResources gates the subsidy on
// `len(stx.Operations) == 1`, so two ops always count.
func TestRCPluginManyRecoverAccounts(t *testing.T) {
	var pubKey [33]byte
	pubKey[0], pubKey[1] = 0x02, 'A'
	stx := newSubsidizedRecoverAccountTx("alice", pubKey, 2)
	// Append a second recover_account op.
	stx.Operations = append(stx.Operations, op.FromOperation(op.RecoverAccount{
		AccountToRecover:   "bob",
		NewOwnerAuthority:  newAuth(pubKey),
		RecentOwnerAuthority: newAuth(pubKey),
	}))
	var result [objects.RCNumResources]int64
	chain.CountResources(stx, int64(80), &result)
	// With 2 ops the subsidy is denied → history/state bytes nonzero.
	if result[objects.RCHistoryBytes] == 0 {
		t.Fatalf("2 recover_accounts should be charged history_bytes, got 0")
	}
	if result[objects.RCStateBytes] == 0 {
		t.Fatalf("2 recover_accounts should be charged state_bytes, got 0")
	}
}

// TestRCPluginMultisigRecoverAccount mirrors rc_plugin_tests.cpp::
// rc_multisig_recover_account.
//
// The C++ test loads new_owner_authority with two keys (threshold > 1)
// and asserts the subsidy doesn't apply (subsidy requires single-auth).
// CountResources gates subsidizedOp on
// (num_account_auths + num_key_auths == 1).
func TestRCPluginMultisigRecoverAccount(t *testing.T) {
	var pubKey [33]byte
	pubKey[0], pubKey[1] = 0x02, 'A'
	var pubKey2 [33]byte
	pubKey2[0], pubKey2[1] = 0x02, 'B'
	a := newAuth(pubKey)
	a.KeyAuths = append(a.KeyAuths, auth.KeyAuth{Key: pubKey2, Weight: 1})
	a.WeightThreshold = 2
	stx := &tx.SignedTransaction{
		Transaction: tx.Transaction{
			Operations: []op.Op{op.FromOperation(op.RecoverAccount{
				AccountToRecover:     "alice",
				NewOwnerAuthority:    a,
				RecentOwnerAuthority: a,
			})},
		},
		Signatures: make([][tx.SignatureSize]byte, 2),
	}
	var result [objects.RCNumResources]int64
	chain.CountResources(stx, int64(100), &result)
	// Multisig auth (2 keys) disqualifies subsidy.
	if result[objects.RCHistoryBytes] == 0 {
		t.Fatalf("multisig recover should be charged history_bytes, got 0")
	}
}

// TestRCPluginTxOrderBug mirrors rc_plugin_tests.cpp::rc_tx_order_bug —
// pop_block + reapply pending transaction interaction with the RC
// plugin. The Go fixture has no pop_block support; this test stays
// skipped with a VERY specific marker (no Phase 5 work unblocks it).
func TestRCPluginTxOrderBug(t *testing.T) {
	t.Skip("MISSING: pop_block / reapply-pending support in testfixture (orthogonal to Phase 5 — the cost-deduction path is in place; the missing piece is rewind+reapply, which lives in chain.Database)")
}

// TestRCPluginPendingDataReset mirrors rc_plugin_tests.cpp::
// rc_pending_data_reset. The C++ test asserts rc_pending_data's
// PendingUsage/PendingCost grow per tx, reset at block boundary, and
// DifferentialUsage resets at every tx. Our Phase 5 core implements
// the per-block reset + per-tx reset of differential — but the per-op
// differential VISITOR is deferred (see file-level header comment), so
// the differential-direction assertions are relaxed to "non-positive".
func TestRCPluginPendingDataReset(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF26)
	f.Actors("alice", "bob")
	f.FundVesting("alice", 1_000_000)
	f.FundLiquid("alice", 100_000)
	f.GenerateBlock()

	// At the start of the block (right after GenerateBlock above which
	// already flushed any prior tx data), pending usage and cost should
	// be zero.
	pre := readRCPendingData(t, f)
	for i := 0; i < objects.RCNumResources; i++ {
		if pre.PendingUsage[i] != 0 {
			t.Fatalf("pending_usage[%d] = %d, want 0 at block boundary",
				i, pre.PendingUsage[i])
		}
		if pre.PendingCost[i] != 0 {
			t.Fatalf("pending_cost[%d] = %d, want 0 at block boundary",
				i, pre.PendingCost[i])
		}
	}

	// Push a transfer — pending_usage should accumulate without flushing
	// yet. (ApplyOp runs the tx-apply path inside one block, then closes
	// the block. The flush is in the block-finalize hook.)
	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "bob", Amount: HIVE(1), Memo: "p",
	}); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	// After the block: flush has fired → PendingUsage is reset to 0;
	// PendingCost is reset to 0; the active bucket's Used array picked
	// up the per-tx usage.
	post := readRCPendingData(t, f)
	for i := 0; i < objects.RCNumResources; i++ {
		if post.PendingUsage[i] != 0 {
			t.Fatalf("post-block pending_usage[%d] = %d, want 0 (flushed)",
				i, post.PendingUsage[i])
		}
	}
	// DifferentialUsage starts at zero pre-tx-reset; per-op visitor is
	// deferred so it stays zero throughout.
	for i := 0; i < objects.RCNumResources; i++ {
		if post.DifferentialUsage[i] > 0 {
			t.Fatalf("differential_usage[%d] = %d > 0; per-op visitor deferred → must stay ≤ 0",
				i, post.DifferentialUsage[i])
		}
	}
}

// readRCPendingData fetches the rc_pending_data singleton.
func readRCPendingData(t *testing.T, f *Fixture) objects.RCPendingData {
	t.Helper()
	return objects.GetRCPendingData(state.NewSession(f.State))
}

// TestRCPluginDifferentialUsageOperations mirrors rc_plugin_tests.cpp::
// rc_differential_usage_operations. The C++ test exercises the per-op
// differential visitor on account_update / account_update2 /
// recover_account / witness_update / delegate_rc — each operation
// shrinking pre-existing state shows a negative differential.
//
// The per-op differential visitor is deferred in Phase 5's "minimum
// viable core" (see rc_phase5.go header). The visitor needs scratch
// state lookups (find<account_authority_object>, find<witness_object>,
// find<rc_direct_delegation_object>) at the pre-apply-OP hook — a
// non-trivial wiring step that's orthogonal to the cost-deduction path.
func TestRCPluginDifferentialUsageOperations(t *testing.T) {
	t.Skip("MISSING: per-op differential visitor (count_differential_operation_visitor in resource_count.cpp). Phase 5 core implements per-tx reset + pending_data accumulator but the visitor hook itself is deferred to Phase 5.1.")
}

// TestRCPluginDifferentialUsageNegative — same missing piece.
func TestRCPluginDifferentialUsageNegative(t *testing.T) {
	t.Skip("MISSING: per-op differential visitor (see TestRCPluginDifferentialUsageOperations).")
}

// TestRCPluginDifferentialUsageManyOps — same missing piece, plus the
// per-OP (not per-TX) granularity that requires hooking the visitor
// inside the per-op apply loop in database.go.
func TestRCPluginDifferentialUsageManyOps(t *testing.T) {
	t.Skip("MISSING: per-op differential visitor + per-op (not per-tx) hook in apply loop (see TestRCPluginDifferentialUsageOperations).")
}

// TestRCPluginExceptionDuringModify mirrors rc_plugin_tests.cpp::
// rc_exception_during_modify. Verifies that when EnforceRC is on
// (skip_reject_not_enough_rc=0) and the payer has 0 RC, a transfer
// is rejected with an error mentioning the payer's name.
func TestRCPluginExceptionDuringModify(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF26)
	f.Actors("dave")
	f.FundVesting("dave", 70_000_000)
	f.FundLiquid("dave", 1_000_000)
	f.GenerateBlock()

	// Clear RC on dave — pin both current and max to zero so any tx
	// cost > 0 triggers the insufficient-RC path.
	s := state.NewSession(f.State)
	dave, _ := objects.GetAccount(s, "dave")
	dave.RCManaCurrent = 0
	dave.VestingShares = 0
	dave.LastMaxRC = 0
	dave.RCManaLastUpdate = f.HeadBlockTime()
	objects.PutAccount(s, &dave)
	if err := s.Commit(false); err != nil {
		t.Fatalf("clear: %v", err)
	}

	f.EnforceRC = true
	err := f.ApplyOp(op.Transfer{
		From: "dave", To: InitMiner, Amount: HIVE(1), Memo: "test",
	})
	if err == nil {
		t.Fatalf("expected insufficient-RC error, got nil")
	}
	if !errors.Is(err, chain.ErrInsufficientRC) {
		t.Fatalf("expected chain.ErrInsufficientRC, got: %v", err)
	}
	if !strings.Contains(err.Error(), "dave") {
		t.Fatalf("error should mention payer 'dave', got: %v", err)
	}
}

// newSubsidizedRecoverAccountTx is a fixture helper that builds a
// signed_transaction containing exactly one recover_account_operation
// with a single-key new_owner_authority. Numbered signatures are
// padded to the requested count.
func newSubsidizedRecoverAccountTx(account string, key [33]byte, sigCount int) *tx.SignedTransaction {
	a := newAuth(key)
	stx := &tx.SignedTransaction{
		Transaction: tx.Transaction{
			Operations: []op.Op{op.FromOperation(op.RecoverAccount{
				AccountToRecover:     auth.AccountName(account),
				NewOwnerAuthority:    a,
				RecentOwnerAuthority: a,
			})},
		},
		Signatures: make([][tx.SignatureSize]byte, sigCount),
	}
	return stx
}
