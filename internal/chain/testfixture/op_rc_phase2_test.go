package testfixture

import (
	"testing"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Phase 2 ports of direct_rc_delegation.cpp and rc_tests.cpp cases that
// exercise the per-block hooks (expired_delegation drain, bucket
// rotation, pool recompute, stats archival, vesting-withdrawal RC
// impact).
//
// The exact-equality assertions in the C++ tests depend on
// `max_rc_creation_adjustment` and per-tx cost deduction (Phase 5 in
// the Go port). Where the C++ test pinned a count derived from
// `creation_rc` or `cost(N)`, we relax to a structural assertion:
// "the row exists / the row was removed / the field was clamped at 0".
// The Phase 5 follow-up will tighten these to exact numbers.

// rcAdvanceBlocks generates n empty blocks. Used to walk the chain
// past a withdrawal interval or a usage-bucket rotation boundary.
func rcAdvanceBlocks(f *Fixture, n int) {
	for i := 0; i < n; i++ {
		f.GenerateBlock()
	}
}

// TestRCUpdateOutdelOverflow exercises the
// direct_rc_delegation.cpp::update_outdel_overflow shape: after a
// delegate_vesting_shares chops alice's effective vesting below her
// outstanding RC delegations, the surplus is expired (the by-from row
// gets clipped and the surplus moves into the expired-delegation queue).
//
// Phase 2 hook validated: handleVestingWithdrawalRCImpact.
func TestRCUpdateOutdelOverflow(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF26)
	f.Actors("alice", "bob", "dave")
	f.FundVesting("alice", 10_000)
	f.GenerateBlock()

	aliceVests := f.Vests("alice")
	// alice delegates ~80% of her vests across bob+dave
	per := aliceVests * 4 / 10
	if err := f.ApplyOp(rcDelegationCustomOp("alice", []string{"bob"}, per)); err != nil {
		t.Fatalf("delegate to bob: %v", err)
	}
	if err := f.ApplyOp(rcDelegationCustomOp("alice", []string{"dave"}, per)); err != nil {
		t.Fatalf("delegate to dave: %v", err)
	}
	if got := f.Account_("alice").DelegatedRCOut; got != per*2 {
		t.Fatalf("alice.DelegatedRCOut: got %d, want %d", got, per*2)
	}

	// Synthetically slash alice's vesting so the delegations now exceed
	// her cap, then run the Phase 2 RC-impact handler. This mirrors the
	// effect of a single vesting-withdrawal chunk in the C++ test.
	s := state.NewSession(f.State)
	alice, _ := objects.GetAccount(s, "alice")
	// Drop alice's vests to half the delegated amount — should clip
	// the first delegation oldest-first.
	cut := per // remove `per` units of vesting
	alice.VestingShares -= cut
	// Re-run the hook (would normally be invoked from processOneWithdrawal).
	handleVestingWithdrawalRCImpactForTest(s, &alice, f.HeadBlockTime())
	objects.PutAccount(s, &alice)
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Post-condition: alice.DelegatedRCOut + expired = per*2 - cut (some
	// of the delegations got clipped, the rest moved to expired-row).
	post := f.Account_("alice")
	if post.DelegatedRCOut > post.VestingShares {
		t.Fatalf("alice.DelegatedRCOut (%d) exceeds VestingShares (%d) after handler",
			post.DelegatedRCOut, post.VestingShares)
	}

	// An RCExpiredDelegation row should now exist for alice.
	s = state.NewSession(f.State)
	if !objects.HasRCExpiredDelegation(s, auth.AccountName("alice")) {
		t.Fatalf("expected RCExpiredDelegation row for alice after vest reduction")
	}
	exp, err := objects.GetRCExpiredDelegation(s, "alice")
	if err != nil {
		t.Fatalf("get expired: %v", err)
	}
	if exp.RemainingExpired <= 0 {
		t.Fatalf("expired.RemainingExpired must be > 0, got %d", exp.RemainingExpired)
	}
	if exp.ExpiryTime == 0 {
		t.Fatalf("expired.ExpiryTime must be set")
	}
}

// TestRCUpdateOutdelOverflowManyAccounts mirrors
// direct_rc_delegation.cpp::update_outdel_overflow_many_accounts. The
// expired-row drain is rate-limited at 100 entries per block; we seed
// 150 rows and verify processRCExpiredDelegations only drains 100 in
// one pass.
func TestRCUpdateOutdelOverflowManyAccounts(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF26)
	f.GenerateBlock()

	// Seed 150 expired rows directly. Each row is for a unique
	// from-account; the per-block batch cap is 100, so two blocks of
	// processing should be required to drain all 150.
	s := state.NewSession(f.State)
	now := f.HeadBlockTime()
	for i := 0; i < 150; i++ {
		name := makeAcctName(i)
		objects.PutAccount(s, &objects.Account{Name: auth.AccountName(name),
			VestingShares: 1_000_000, DelegatedRCOut: 100})
		objects.PutRCExpiredDelegation(s, &objects.RCExpiredDelegation{
			From:             auth.AccountName(name),
			RemainingExpired: 100,
			ExpiryTime:       now - 10, // already expired
		})
	}
	if err := s.Commit(false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Run the hook once.
	s = state.NewSession(f.State)
	processRCExpiredDelegationsForTest(s, now)
	if err := s.Commit(false); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// Count remaining.
	s = state.NewSession(f.State)
	remaining := 0
	_ = s.Range(objects.RCExpiredDelegationTable, state.PrimaryIndex, nil,
		func(_, _ []byte) (bool, error) {
			remaining++
			return true, nil
		})
	if remaining != 50 {
		t.Fatalf("expected 50 rows remaining after rate-limited drain, got %d", remaining)
	}

	// Run again — should drain the rest.
	s = state.NewSession(f.State)
	processRCExpiredDelegationsForTest(s, now)
	if err := s.Commit(false); err != nil {
		t.Fatalf("drain2: %v", err)
	}
	s = state.NewSession(f.State)
	remaining = 0
	_ = s.Range(objects.RCExpiredDelegationTable, state.PrimaryIndex, nil,
		func(_, _ []byte) (bool, error) {
			remaining++
			return true, nil
		})
	if remaining != 0 {
		t.Fatalf("expected 0 rows after second drain, got %d", remaining)
	}
}

// makeAcctName produces a deterministic 5-16 char account name from i.
// Used by tests that need many distinct accounts.
func makeAcctName(i int) string {
	// "rcacct000"..."rcacct999" — 9 chars, fits HF26 account-name limits.
	out := []byte("rcacct")
	d := []byte{
		byte('0' + (i/100)%10),
		byte('0' + (i/10)%10),
		byte('0' + i%10),
	}
	return string(append(out, d...))
}

// TestRCUsageBucketRotation exercises rotateRCUsageBuckets across a
// 3600-second boundary. The HF26 fixture init has already seeded
// RCWindowBucketCount empty buckets; first rotation drops the oldest
// and appends a new one.
func TestRCUsageBucketRotation(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF26)
	f.GenerateBlock()

	// HF26 fixture init seeds 24 buckets with Created=fixture-time.
	// Read existing max seqno + newest Created so we can pick a 'now'
	// past the rotation guard.
	s := state.NewSession(f.State)
	count := 0
	maxSeq := uint32(0)
	newestCreated := uint32(0)
	_ = s.Range(objects.RCUsageBucketTable, state.PrimaryIndex, nil,
		func(_, val []byte) (bool, error) {
			b, _ := objects.ReadRCUsageBucket(val)
			count++
			if b.BucketSeqno > maxSeq {
				maxSeq = b.BucketSeqno
				newestCreated = b.Created
			}
			return true, nil
		})
	if count != 24 {
		t.Fatalf("expected 24 seeded buckets, got %d", count)
	}
	if maxSeq != 24 {
		t.Fatalf("expected max seqno 24, got %d", maxSeq)
	}

	// Advance time past 3600s after newestCreated and rotate.
	rotateTime := newestCreated + 3700
	s = state.NewSession(f.State)
	rotateRCUsageBucketsForTest(s, rotateTime)
	if err := s.Commit(false); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// Now we should still have 24 buckets, but max seqno = 25 and the
	// seqno=1 bucket is gone.
	s = state.NewSession(f.State)
	count = 0
	maxSeq = 0
	minSeq := uint32(1 << 31)
	_ = s.Range(objects.RCUsageBucketTable, state.PrimaryIndex, nil,
		func(_, val []byte) (bool, error) {
			b, _ := objects.ReadRCUsageBucket(val)
			count++
			if b.BucketSeqno > maxSeq {
				maxSeq = b.BucketSeqno
			}
			if b.BucketSeqno < minSeq {
				minSeq = b.BucketSeqno
			}
			return true, nil
		})
	if count != 24 {
		t.Fatalf("post-rotation: expected 24 buckets, got %d", count)
	}
	if maxSeq != 25 {
		t.Fatalf("post-rotation: expected max seqno 25, got %d", maxSeq)
	}
	if minSeq != 2 {
		t.Fatalf("post-rotation: expected min seqno 2, got %d", minSeq)
	}
}

// TestRCPoolPendingDataReset mirrors rc_tests.cpp::rc_pending_data_reset
// in shape. The pending rc_stats row is reset every
// HIVE_RC_STATS_REPORT_FREQUENCY/3 blocks (= 9600 blocks). We poke a
// pending row, call finalizeRCStats at the boundary, and verify the
// archive is written and pending is zeroed.
func TestRCPoolPendingDataReset(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF26)
	f.GenerateBlock()

	// Seed a pending row with some activity.
	s := state.NewSession(f.State)
	pending := objects.RCStats{
		BlockNum: 100,
		Regen:    123,
		Budget:   [objects.RCNumResources]int64{1, 2, 3, 4, 5},
	}
	objects.PutRCStats(s, objects.RCStatsIDPending, &pending)
	if err := s.Commit(false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Fire at a non-boundary block — should be a no-op.
	s = state.NewSession(f.State)
	finalizeRCStatsForTest(s, 100)
	if err := s.Commit(false); err != nil {
		t.Fatalf("noop fire: %v", err)
	}
	s = state.NewSession(f.State)
	if _, err := objects.GetRCStats(s, objects.RCStatsIDArchive); err == nil {
		t.Fatalf("archive should not exist before boundary")
	}

	// Fire at the boundary block (= 9600).
	s = state.NewSession(f.State)
	finalizeRCStatsForTest(s, 9600)
	if err := s.Commit(false); err != nil {
		t.Fatalf("boundary fire: %v", err)
	}

	// Archive should now match the pre-reset pending.
	s = state.NewSession(f.State)
	arch, err := objects.GetRCStats(s, objects.RCStatsIDArchive)
	if err != nil {
		t.Fatalf("archive missing after boundary: %v", err)
	}
	if arch.Regen != 123 {
		t.Fatalf("archive.Regen: got %d, want 123", arch.Regen)
	}
	if arch.Budget[2] != 3 {
		t.Fatalf("archive.Budget[2]: got %d, want 3", arch.Budget[2])
	}

	// Pending should be reset (Regen back to 0).
	pendingNow, err := objects.GetRCStats(s, objects.RCStatsIDPending)
	if err != nil {
		t.Fatalf("pending missing after reset: %v", err)
	}
	if pendingNow.Regen != 0 {
		t.Fatalf("pending.Regen after reset: got %d, want 0", pendingNow.Regen)
	}
	if pendingNow.BlockNum != 9600 {
		t.Fatalf("pending.BlockNum after reset: got %d, want 9600", pendingNow.BlockNum)
	}
}

// TestRCDelegationRegenerationStub is the regeneration-timing case from
// direct_rc_delegation.cpp. Needs Phase 5 (per-tx RC cost deduction) to
// actually drain mana for the regeneration window — skipped with marker.
func TestRCDelegationRegenerationStub(t *testing.T) {
	t.Skip("Phase 5: rc_delegation_regeneration requires per-tx cost deduction in replay")
}

// TestRCNegativeRegenerationBugStub is the negative-mana protection
// case. Needs Phase 5 cost deduction + Phase 2 expired drain combined.
func TestRCNegativeRegenerationBugStub(t *testing.T) {
	t.Skip("Phase 5 + Phase 2: rc_negative_regeneration_bug requires combined drain + cost")
}

// TestRCDirectRCDelegationVestingWithdrawalStub — the exact-numbered
// version of TestRCUpdateOutdelOverflow that walks the full 13-week
// power-down across HIVE_VESTING_WITHDRAW_INTERVALS. The structural
// behavior is exercised by TestRCUpdateOutdelOverflow above; the
// cost-precise assertions need Phase 5.
func TestRCDirectRCDelegationVestingWithdrawalStub(t *testing.T) {
	t.Skip("Phase 5: direct_rc_delegation_vesting_withdrawal needs creation_rc + per-tx cost")
}

// TestRCDirectRCDelegationVestingWithdrawalRoutesStub — same shape
// with withdraw_routes redirecting a fraction of each chunk.
func TestRCDirectRCDelegationVestingWithdrawalRoutesStub(t *testing.T) {
	t.Skip("Phase 5: direct_rc_delegation_vesting_withdrawal_routes needs Phase 5")
}

// TestRCUpdateOutdelOverflowDelegateeStub — overflow caused by the
// delegatee's vest reduction (less common path).
func TestRCUpdateOutdelOverflowDelegateeStub(t *testing.T) {
	t.Skip("Phase 5: update_outdel_overflow_delegatee needs creation_rc tracking")
}

// TestRCUpdateOutdelOverflowDelegateePerformanceStub — many-delegations
// perf case; structural correctness is covered by ManyAccounts above.
func TestRCUpdateOutdelOverflowDelegateePerformanceStub(t *testing.T) {
	t.Skip("Phase 5/perf: update_outdel_overflow_delegatee_performance not portable without Phase 5")
}

// TestRCUsageBucketsAdvancedStub — rc_tests.cpp::rc_usage_buckets full
// case with actual ops driving usage; basic rotation is tested in
// TestRCUsageBucketRotation above.
func TestRCUsageBucketsAdvancedStub(t *testing.T) {
	t.Skip("Phase 5: rc_usage_buckets per-op cost accounting requires Phase 5")
}
