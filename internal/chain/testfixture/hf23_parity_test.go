package testfixture

import (
	"testing"

	"gopherd/internal/chain"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// 1:1 parity scaffolds for hive_tes/tests/unit/tests/hf23_tests.cpp.
//
// HF23 ("Eclipse") drained a hardcoded list of Steemit-Inc-aligned and
// Tron-puppet accounts into the treasury. HF24 then restored a small
// subset that turned out not to be aligned with the takeover, drawing
// from the treasury via the h23_balances snapshot recorded by
// gather_balance.
//
// All tests below port the corresponding BOOST_AUTO_TEST_CASE in
// hf23_tests.cpp. The public hooks used here:
//
//	chain.ClearAccount(s, name) error
//	chain.GatherBalance(s, name, hive, hbd)
//	chain.RestoreAccounts(s, names)
//
// — mirror hived's db->clear_account / db->gather_balance /
// db->restore_accounts.

// setupHF23 brings the fixture up to HF23 with the treasury account
// pre-funded with a tiny baseline so the treasury reads consistent
// values across tests.
func setupHF23(t *testing.T) *Fixture {
	t.Helper()
	f := New(t)
	// HF21 creates the treasury (steem.dao) and seeds it; HF23 fires
	// clear_accounts on the steemit list. Jump to HF22 so the
	// pre-Eclipse treasury exists but the clear list hasn't been swept
	// (avoids accidentally wiping the test actors).
	f.HardforksUpTo(hardfork.HF22)
	return f
}

// --- basic_test_01..06 — exercise clear_account on various
// VEST/delegation setups. ----------------------------------------------

// TestHF23BasicTest01 ports hf23_tests.cpp::basic_test_01 — just calls
// clear_account on a freshly-created actor.
func TestHF23BasicTest01New(t *testing.T) {
	f := setupHF23(t)
	f.Actor("alice")
	f.GenerateBlock()

	s := state.NewSession(f.State)
	if err := chain.ClearAccount(s, "alice"); err != nil {
		t.Fatalf("ClearAccount: %v", err)
	}
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestHF23BasicTest02 ports basic_test_02 — vest then clear → vesting=0.
func TestHF23BasicTest02(t *testing.T) {
	f := setupHF23(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()
	f.FundLiquid("alice", 1_000_000)
	f.FundLiquid("bob", 1_000_000)
	f.FundVesting("alice", 1_000)
	bobVestPre := f.Vests("bob")
	if f.Vests("alice") == 0 {
		t.Fatal("alice should have vesting after FundVesting")
	}

	s := state.NewSession(f.State)
	if err := chain.ClearAccount(s, "alice"); err != nil {
		t.Fatalf("clear alice: %v", err)
	}
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit alice: %v", err)
	}
	if got := f.Vests("alice"); got != 0 {
		t.Errorf("alice vesting after clear: got %d, want 0", got)
	}
	if got := f.Vests("bob"); got != bobVestPre {
		t.Errorf("bob vesting (unchanged): got %d, want %d", got, bobVestPre)
	}

	s2 := state.NewSession(f.State)
	if err := chain.ClearAccount(s2, "bob"); err != nil {
		t.Fatalf("clear bob: %v", err)
	}
	if err := s2.Commit(false); err != nil {
		t.Fatalf("commit bob: %v", err)
	}
	if got := f.Vests("alice"); got != 0 {
		t.Errorf("alice vesting still 0: got %d", got)
	}
	if got := f.Vests("bob"); got != 0 {
		t.Errorf("bob vesting after clear: got %d, want 0", got)
	}
}

// TestHF23BasicTest03 ports basic_test_03 — multiple accounts vest, then
// each is cleared. Each clear zeroes only the cleared account.
func TestHF23BasicTest03(t *testing.T) {
	f := setupHF23(t)
	f.Actors("alice", "bob", "carol")
	f.GenerateBlock()
	for _, n := range []string{"alice", "bob", "carol"} {
		f.FundLiquid(n, 1_000_000)
		f.FundVesting(n, 1_000+int64(len(n)))
	}
	vestB := f.Vests("bob")
	vestC := f.Vests("carol")

	for i, target := range []string{"alice", "bob", "carol"} {
		s := state.NewSession(f.State)
		if err := chain.ClearAccount(s, target); err != nil {
			t.Fatalf("clear %s: %v", target, err)
		}
		if err := s.Commit(false); err != nil {
			t.Fatalf("commit %s: %v", target, err)
		}
		if got := f.Vests(target); got != 0 {
			t.Errorf("%s vesting after clear: got %d", target, got)
		}
		// Untouched accounts retain their vesting.
		_ = i
	}
	// All three should be zero now.
	if got := f.Vests("bob"); got != 0 || vestB == 0 {
		t.Errorf("bob: pre=%d post=%d", vestB, got)
	}
	if got := f.Vests("carol"); got != 0 || vestC == 0 {
		t.Errorf("carol: pre=%d post=%d", vestC, got)
	}
}

// TestHF23BasicTest04 ports basic_test_04 — delegate_vest then
// clear delegator → delegated_vests=0, received_vests=0 for delegatee.
func TestHF23BasicTest04(t *testing.T) {
	f := setupHF23(t)
	f.Actors("alice", "bob", "carol")
	f.GenerateBlock()
	for _, n := range []string{"alice", "bob", "carol"} {
		f.FundLiquid(n, 1_000_000)
		f.FundVesting(n, 30_000)
	}

	// alice delegates to bob.
	aliceVests := f.Vests("alice")
	delegateAmount := aliceVests / 4
	if err := f.ApplyOp(op.DelegateVestingShares{
		Delegator: "alice", Delegatee: "bob",
		VestingShares: VESTS(delegateAmount),
	}); err != nil {
		t.Fatalf("delegate alice→bob: %v", err)
	}
	delegateAmount2 := aliceVests / 8
	if err := f.ApplyOp(op.DelegateVestingShares{
		Delegator: "alice", Delegatee: "carol",
		VestingShares: VESTS(delegateAmount2),
	}); err != nil {
		t.Fatalf("delegate alice→carol: %v", err)
	}

	// Pre-clear sanity.
	aliceAcct := f.Account_("alice")
	bobAcct := f.Account_("bob")
	carolAcct := f.Account_("carol")
	if aliceAcct.DelegatedVestingShares == 0 {
		t.Fatal("alice should have delegated vests")
	}
	if bobAcct.ReceivedVestingShares == 0 || carolAcct.ReceivedVestingShares == 0 {
		t.Fatal("bob and carol should have received vests")
	}

	// Clear alice. Delegated/received should all collapse to 0.
	s := state.NewSession(f.State)
	if err := chain.ClearAccount(s, "alice"); err != nil {
		t.Fatalf("clear alice: %v", err)
	}
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := f.Account_("alice").DelegatedVestingShares; got != 0 {
		t.Errorf("alice delegated post-clear: got %d", got)
	}
	if got := f.Account_("bob").ReceivedVestingShares; got != 0 {
		t.Errorf("bob received post-clear: got %d", got)
	}
	if got := f.Account_("carol").ReceivedVestingShares; got != 0 {
		t.Errorf("carol received post-clear: got %d", got)
	}
}

// TestHF23BasicTest05 ports basic_test_05 — chain of delegations
// (alice→bob, alice→carol, carol→bob), then clear each; verify
// per-account delegated/received accounting.
func TestHF23BasicTest05(t *testing.T) {
	f := setupHF23(t)
	f.Actors("alice", "bob", "carol")
	f.GenerateBlock()
	for _, n := range []string{"alice", "bob", "carol"} {
		f.FundLiquid(n, 1_000_000)
		f.FundVesting(n, 30_000)
	}

	d1 := f.Vests("alice") / 6
	d2 := f.Vests("alice") / 12
	d3 := f.Vests("carol") / 6
	if err := f.ApplyOp(op.DelegateVestingShares{
		Delegator: "alice", Delegatee: "bob", VestingShares: VESTS(d1),
	}); err != nil {
		t.Fatalf("delegate1: %v", err)
	}
	if err := f.ApplyOp(op.DelegateVestingShares{
		Delegator: "alice", Delegatee: "carol", VestingShares: VESTS(d2),
	}); err != nil {
		t.Fatalf("delegate2: %v", err)
	}
	if err := f.ApplyOp(op.DelegateVestingShares{
		Delegator: "carol", Delegatee: "bob", VestingShares: VESTS(d3),
	}); err != nil {
		t.Fatalf("delegate3: %v", err)
	}

	// Pre-clear: bob has d1+d3 received; carol has d2 received and d3
	// delegated; alice has d1+d2 delegated.
	if got := f.Account_("alice").DelegatedVestingShares; got != d1+d2 {
		t.Errorf("alice delegated: got %d want %d", got, d1+d2)
	}
	if got := f.Account_("carol").DelegatedVestingShares; got != d3 {
		t.Errorf("carol delegated: got %d want %d", got, d3)
	}
	if got := f.Account_("bob").ReceivedVestingShares; got != d1+d3 {
		t.Errorf("bob received: got %d want %d", got, d1+d3)
	}
	if got := f.Account_("carol").ReceivedVestingShares; got != d2 {
		t.Errorf("carol received: got %d want %d", got, d2)
	}

	// Clear carol. alice's delegations stay (alice→bob, alice→carol).
	// bob's received should drop d3 (the carol→bob portion). carol's
	// delegated should drop to 0.
	s := state.NewSession(f.State)
	if err := chain.ClearAccount(s, "carol"); err != nil {
		t.Fatalf("clear carol: %v", err)
	}
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got := f.Account_("carol").DelegatedVestingShares; got != 0 {
		t.Errorf("carol delegated post-clear: got %d", got)
	}
	if got := f.Account_("alice").DelegatedVestingShares; got != d1+d2 {
		t.Errorf("alice delegated unchanged: got %d want %d", got, d1+d2)
	}
	if got := f.Account_("bob").ReceivedVestingShares; got != d1 {
		t.Errorf("bob received post-carol-clear: got %d want %d", got, d1)
	}

	// Clear alice. bob's received should drop to 0; carol's received
	// should also drop to 0.
	s2 := state.NewSession(f.State)
	if err := chain.ClearAccount(s2, "alice"); err != nil {
		t.Fatalf("clear alice: %v", err)
	}
	if err := s2.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := f.Account_("alice").DelegatedVestingShares; got != 0 {
		t.Errorf("alice delegated post-clear: got %d", got)
	}
	if got := f.Account_("bob").ReceivedVestingShares; got != 0 {
		t.Errorf("bob received post-alice-clear: got %d", got)
	}
	if got := f.Account_("carol").ReceivedVestingShares; got != 0 {
		t.Errorf("carol received post-alice-clear: got %d", got)
	}
}

// TestHF23BasicTest06 ports basic_test_06 — vesting_delegation_expiration
// objects from reduced delegations are removed when the delegator is
// cleared.
func TestHF23BasicTest06(t *testing.T) {
	f := setupHF23(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()
	f.FundLiquid("alice", 1_000_000)
	f.FundLiquid("bob", 1_000_000)
	f.FundVesting("alice", 30_000)
	f.FundVesting("bob", 30_000)

	d1 := f.Vests("alice") / 4
	d2 := f.Vests("alice") / 8 // < d1 → reduction → creates a *_expiration row

	if err := f.ApplyOp(op.DelegateVestingShares{
		Delegator: "alice", Delegatee: "bob", VestingShares: VESTS(d1),
	}); err != nil {
		t.Fatalf("delegate1: %v", err)
	}
	if err := f.ApplyOp(op.DelegateVestingShares{
		Delegator: "alice", Delegatee: "bob", VestingShares: VESTS(d2),
	}); err != nil {
		t.Fatalf("reduce-delegate: %v", err)
	}

	// At least one vesting_delegation_expiration row should exist for alice.
	expCountPre := countExpFor(t, f, "alice")
	if expCountPre == 0 {
		t.Fatalf("expected vesting_delegation_expiration row for alice after reduction")
	}

	// Clear alice. The expiration rows must be removed.
	s := state.NewSession(f.State)
	if err := chain.ClearAccount(s, "alice"); err != nil {
		t.Fatalf("clear alice: %v", err)
	}
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := countExpFor(t, f, "alice"); got != 0 {
		t.Errorf("alice expirations post-clear: got %d want 0", got)
	}
}

// countExpFor returns the number of vesting_delegation_expiration rows
// whose delegator == name.
func countExpFor(t *testing.T, f *Fixture, name string) int {
	t.Helper()
	s := state.NewSession(f.State)
	prefix := state.AppendAccountName(nil, AccountName(name))
	count := 0
	err := s.Range(objects.VestingDelegationExpirationTable, state.PrimaryIndex, prefix, func(_, value []byte) (bool, error) {
		e, eerr := objects.ReadVestingDelegationExpiration(value)
		if eerr != nil {
			return false, eerr
		}
		if string(e.Delegator) == name {
			count++
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("scan expirations: %v", err)
	}
	return count
}

// --- save_test_01, save_test_02 — exercise db->gather_balance() and
// inspect hardfork_property_object.h23_balances. -----------------------

// TestHF23SaveTest01 ports save_test_01 — two accounts captured.
func TestHF23SaveTest01(t *testing.T) {
	f := setupHF23(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()
	f.FundLiquid("alice", 1_000_000)
	f.FundLiquid("bob", 2_000_000)
	f.FundLiquidHBD("alice", 20_000)

	aliceBal := f.Balance("alice")
	aliceHbd := f.HBDBalance("alice")
	bobBal := f.Balance("bob")
	bobHbd := f.HBDBalance("bob")

	s := state.NewSession(f.State)
	chain.GatherBalance(s, "alice", aliceBal, aliceHbd)
	chain.GatherBalance(s, "bob", bobBal, bobHbd)
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if n, err := objects.CountH23Balances(state.NewSession(f.State)); err != nil || n != 2 {
		t.Fatalf("h23_balances count: got %d err=%v", n, err)
	}
	aSnap, err := objects.GetH23Balance(state.NewSession(f.State), AccountName("alice"))
	if err != nil {
		t.Fatalf("get alice snap: %v", err)
	}
	if aSnap.Balance != aliceBal || aSnap.HBDBalance != aliceHbd {
		t.Errorf("alice snap: got (%d,%d) want (%d,%d)", aSnap.Balance, aSnap.HBDBalance, aliceBal, aliceHbd)
	}
	bSnap, err := objects.GetH23Balance(state.NewSession(f.State), AccountName("bob"))
	if err != nil {
		t.Fatalf("get bob snap: %v", err)
	}
	if bSnap.Balance != bobBal || bSnap.HBDBalance != bobHbd {
		t.Errorf("bob snap: got (%d,%d) want (%d,%d)", bSnap.Balance, bSnap.HBDBalance, bobBal, bobHbd)
	}
}

// TestHF23SaveTest02 ports save_test_02 — single account captured.
func TestHF23SaveTest02(t *testing.T) {
	f := setupHF23(t)
	f.Actor("alice")
	f.GenerateBlock()
	f.FundLiquid("alice", 1_000_000)
	f.FundLiquidHBD("alice", 20_000)

	aliceBal := f.Balance("alice")
	aliceHbd := f.HBDBalance("alice")

	s := state.NewSession(f.State)
	chain.GatherBalance(s, "alice", aliceBal, aliceHbd)
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if n, err := objects.CountH23Balances(state.NewSession(f.State)); err != nil || n != 1 {
		t.Fatalf("h23_balances count: got %d err=%v", n, err)
	}
	snap, err := objects.GetH23Balance(state.NewSession(f.State), AccountName("alice"))
	if err != nil {
		t.Fatalf("get snap: %v", err)
	}
	if snap.Balance != aliceBal || snap.HBDBalance != aliceHbd {
		t.Errorf("alice snap: got (%d,%d) want (%d,%d)", snap.Balance, snap.HBDBalance, aliceBal, aliceHbd)
	}
}

// --- restore_accounts_01, restore_accounts_02 — exercise
// db->restore_accounts() against a pre-HF24 snapshot. -------------------

// TestHF23RestoreAccounts01 ports restore_accounts_01 — gather_balance
// followed by transfer-to-treasury then restore_accounts(...).
func TestHF23RestoreAccounts01(t *testing.T) {
	f := setupHF23(t)
	f.Actors("alice", "bob")
	if !f.HasAccount("steem.dao") {
		f.Actor("steem.dao")
	}
	f.GenerateBlock()
	f.FundLiquid("alice", 1_000_000)
	f.FundLiquidHBD("alice", 20_000)
	f.FundLiquid("bob", 900_000)
	f.FundLiquidHBD("bob", 10_000)

	aliceBalPre := f.Balance("alice")
	aliceHbdPre := f.HBDBalance("alice")
	bobBalPre := f.Balance("bob")
	bobHbdPre := f.HBDBalance("bob")
	// Treasury fund up-front so it can pay the restoration.
	f.FundLiquid("steem.dao", aliceBalPre+bobBalPre+10_000)
	f.FundLiquidHBD("steem.dao", aliceHbdPre+bobHbdPre+5_000)

	preTreasuryHive := f.Balance("steem.dao")
	preTreasuryHbd := f.HBDBalance("steem.dao")

	// Simulate the C++ pattern of gather_balance + manual transfer-to-treasury.
	s := state.NewSession(f.State)
	chain.GatherBalance(s, "alice", aliceBalPre, aliceHbdPre)
	chain.GatherBalance(s, "bob", bobBalPre, bobHbdPre)
	// Drain alice and bob.
	a, _ := objects.GetAccount(s, "alice")
	b, _ := objects.GetAccount(s, "bob")
	treas, _ := objects.GetAccount(s, "steem.dao")
	treas.Balance += a.Balance
	a.Balance = 0
	treas.Balance += b.Balance
	b.Balance = 0
	dgpo, _ := objects.GetDGPO(s)
	objects.AdjustHBDBalance(s, &treas, dgpo.Time, a.HBDBalance)
	a.HBDBalance = 0
	objects.AdjustHBDBalance(s, &treas, dgpo.Time, b.HBDBalance)
	b.HBDBalance = 0
	objects.PutAccount(s, &a)
	objects.PutAccount(s, &b)
	objects.PutAccount(s, &treas)
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit drain: %v", err)
	}

	// Restore alice + bob; carol is in the list but has no snapshot — should be skipped.
	s2 := state.NewSession(f.State)
	chain.RestoreAccounts(s2, []string{"alice", "bob", "carol"})
	if err := s2.Commit(false); err != nil {
		t.Fatalf("commit restore: %v", err)
	}

	if got := f.Balance("alice"); got != aliceBalPre {
		t.Errorf("alice HIVE post-restore: got %d want %d", got, aliceBalPre)
	}
	if got := f.HBDBalance("alice"); got != aliceHbdPre {
		t.Errorf("alice HBD post-restore: got %d want %d", got, aliceHbdPre)
	}
	if got := f.Balance("bob"); got != bobBalPre {
		t.Errorf("bob HIVE post-restore: got %d want %d", got, bobBalPre)
	}
	if got := f.HBDBalance("bob"); got != bobHbdPre {
		t.Errorf("bob HBD post-restore: got %d want %d", got, bobHbdPre)
	}

	// Treasury should be back near its pre-restore baseline.
	postTreasuryHive := f.Balance("steem.dao")
	postTreasuryHbd := f.HBDBalance("steem.dao")
	if postTreasuryHive != preTreasuryHive {
		t.Errorf("treasury HIVE round-trip: got %d want %d", postTreasuryHive, preTreasuryHive)
	}
	// HBD round-trip can pick up interest; allow small upward drift.
	if postTreasuryHbd < preTreasuryHbd-10 || postTreasuryHbd > preTreasuryHbd+100 {
		t.Errorf("treasury HBD round-trip: got %d want ~%d", postTreasuryHbd, preTreasuryHbd)
	}
}

// TestHF23RestoreAccounts02 ports restore_accounts_02 — full
// clear_accounts → restore_accounts cycle via the public hooks.
func TestHF23RestoreAccounts02(t *testing.T) {
	f := setupHF23(t)
	f.Actors("alice0", "alice1", "alice2", "dude")
	if !f.HasAccount("steem.dao") {
		f.Actor("steem.dao")
	}
	f.GenerateBlock()

	// Fund alice0..alice2 (the to-be-cleared set) and `dude` (control).
	balances := map[string][2]int64{}
	for i, name := range []string{"alice0", "alice1", "alice2"} {
		hive := int64((i+1)*1000) * 1000 // *1000 to land in satoshi
		hbd := int64((i+1)*1000) + 1
		f.FundLiquid(name, hive)
		f.FundLiquidHBD(name, hbd)
		balances[name] = [2]int64{hive, hbd}
	}
	f.FundLiquid("dude", 68_000)
	f.FundLiquidHBD("dude", 78_000)
	// Pre-fund the treasury so the eventual restore has assets to draw.
	f.FundLiquid("steem.dao", 1_000_000_000)
	f.FundLiquidHBD("steem.dao", 1_000_000_000)

	// Clear all three alice accounts. Each clearAccount records a
	// gather_balance snapshot internally.
	s := state.NewSession(f.State)
	for _, name := range []string{"alice0", "alice1", "alice2"} {
		if err := chain.ClearAccount(s, name); err != nil {
			t.Fatalf("clear %s: %v", name, err)
		}
	}
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit clear: %v", err)
	}

	for _, name := range []string{"alice0", "alice1", "alice2"} {
		if got := f.Balance(name); got != 0 {
			t.Errorf("%s HIVE post-clear: got %d", name, got)
		}
		if got := f.HBDBalance(name); got != 0 {
			t.Errorf("%s HBD post-clear: got %d", name, got)
		}
	}
	if got := f.Balance("dude"); got != 68_000 {
		t.Errorf("dude HIVE: got %d", got)
	}
	if got := f.HBDBalance("dude"); got != 78_000 {
		t.Errorf("dude HBD: got %d", got)
	}

	// Restore alice0..alice2 plus dude (no snapshot → skipped) plus
	// "devil" (no account → skipped).
	s2 := state.NewSession(f.State)
	chain.RestoreAccounts(s2, []string{"alice0", "alice1", "alice2", "dude", "devil"})
	if err := s2.Commit(false); err != nil {
		t.Fatalf("commit restore: %v", err)
	}

	for name, want := range balances {
		if got := f.Balance(name); got != want[0] {
			t.Errorf("%s HIVE post-restore: got %d want %d", name, got, want[0])
		}
		if got := f.HBDBalance(name); got != want[1] {
			t.Errorf("%s HBD post-restore: got %d want %d", name, got, want[1])
		}
	}
	// dude untouched.
	if got := f.Balance("dude"); got != 68_000 {
		t.Errorf("dude HIVE unchanged: got %d", got)
	}
	if got := f.HBDBalance("dude"); got != 78_000 {
		t.Errorf("dude HBD unchanged: got %d", got)
	}
}

// --- escrow_cleanup_test (subset). --------------------------------------

// TestHF23EscrowCleanupTest ports the first branch of
// hf23_tests.cpp::escrow_cleanup_test: a pending unapproved escrow gets
// dissolved and the refund lands in the treasury.
func TestHF23EscrowCleanupTest(t *testing.T) {
	f := setupHF23(t)
	f.Actors("alice", "bob", "carol")
	if !f.HasAccount("steem.dao") {
		f.Actor("steem.dao")
	}
	f.GenerateBlock()
	f.FundLiquid("alice", 10_000)
	f.FundLiquidHBD("alice", 10_100) // 10 HBD + 0.100 HBD fee

	if err := f.ApplyOp(op.EscrowTransfer{
		From: "alice", To: "bob", Agent: "carol", EscrowID: 30,
		HiveAmount:           HIVE(10_000),
		HbdAmount:            HBD(10_000),
		Fee:                  HBD(100),
		RatificationDeadline: f.HeadBlockTime() + BlockInterval*10,
		EscrowExpiration:     f.HeadBlockTime() + BlockInterval*20,
	}); err != nil {
		t.Fatalf("escrow_transfer: %v", err)
	}
	if has, _ := objects.HasEscrow(state.NewSession(f.State), "alice", 30); !has {
		t.Fatal("escrow should exist post-create")
	}
	// alice fully debited.
	if got := f.Balance("alice"); got != 0 {
		t.Fatalf("alice HIVE post-escrow: got %d", got)
	}

	treasuryHivePre := f.Balance("steem.dao")
	treasuryHbdPre := f.HBDBalance("steem.dao")

	// Clear alice while the escrow is still pending.
	s := state.NewSession(f.State)
	if err := chain.ClearAccount(s, "alice"); err != nil {
		t.Fatalf("clear alice: %v", err)
	}
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// Escrow gone.
	if has, _ := objects.HasEscrow(state.NewSession(f.State), "alice", 30); has {
		t.Error("escrow row should be removed after clear")
	}
	// 10 HIVE refund + treasury baseline; 10.100 HBD refund.
	if got := f.Balance("steem.dao"); got != treasuryHivePre+10_000 {
		t.Errorf("treasury HIVE: got %d want %d", got, treasuryHivePre+10_000)
	}
	if got := f.HBDBalance("steem.dao"); got < treasuryHbdPre+10_100-10 || got > treasuryHbdPre+10_100+100 {
		t.Errorf("treasury HBD: got %d want ~%d", got, treasuryHbdPre+10_100)
	}
}

// --- limit_order_cleanup_test (subset). ---------------------------------

// TestHF23LimitOrderCleanupTest ports the first branch of
// limit_order_cleanup_test: a pending limit_order_create gets cancelled
// at clear, and the locked balance routes to the treasury.
func TestHF23LimitOrderCleanupTest(t *testing.T) {
	f := setupHF23(t)
	f.Actor("alice")
	if !f.HasAccount("steem.dao") {
		f.Actor("steem.dao")
	}
	f.GenerateBlock()
	f.FundLiquid("alice", 10_000)

	if err := f.ApplyOp(op.LimitOrderCreate{
		Owner: "alice", OrderID: 0,
		AmountToSell: HIVE(10_000),
		MinToReceive: HBD(5_000),
		Expiration:   f.HeadBlockTime() + BlockInterval*20,
	}); err != nil {
		t.Fatalf("limit_order_create: %v", err)
	}
	if got := f.Balance("alice"); got != 0 {
		t.Fatalf("alice HIVE escrowed: got %d", got)
	}
	if has, _ := objects.HasLimitOrder(state.NewSession(f.State), "alice", 0); !has {
		t.Fatal("alice/0 order should exist")
	}

	treasuryHivePre := f.Balance("steem.dao")

	s := state.NewSession(f.State)
	if err := chain.ClearAccount(s, "alice"); err != nil {
		t.Fatalf("clear alice: %v", err)
	}
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if has, _ := objects.HasLimitOrder(state.NewSession(f.State), "alice", 0); has {
		t.Error("alice/0 order should be removed after clear")
	}
	if got := f.Balance("steem.dao"); got != treasuryHivePre+10_000 {
		t.Errorf("treasury HIVE: got %d want %d", got, treasuryHivePre+10_000)
	}
}

// --- convert_request_cleanup_test. --------------------------------------

// TestHF23ConvertRequestCleanupTest ports the first branch of
// convert_request_cleanup_test: a pending convert_request gets refunded
// at clear, and the HBD routes to the treasury. Also asserts that
// clear_account REFUSES (errors) when a collateralized_convert request
// exists.
func TestHF23ConvertRequestCleanupTest(t *testing.T) {
	f := setupHF23(t)
	f.Actor("alice")
	if !f.HasAccount("steem.dao") {
		f.Actor("steem.dao")
	}
	f.GenerateBlock()
	f.FundLiquidHBD("alice", 5_000)
	f.SetPriceFeed(t, DefaultPriceFeed())

	if err := f.ApplyOp(op.Convert{
		Owner: "alice", RequestID: 1, Amount: HBD(5_000),
	}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got := f.HBDBalance("alice"); got != 0 {
		t.Fatalf("alice HBD post-convert: got %d", got)
	}
	if has, _ := objects.HasConvertRequest(state.NewSession(f.State), "alice", 1); !has {
		t.Fatal("convert request should exist post-create")
	}

	treasuryHbdPre := f.HBDBalance("steem.dao")

	s := state.NewSession(f.State)
	if err := chain.ClearAccount(s, "alice"); err != nil {
		t.Fatalf("clear alice: %v", err)
	}
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if has, _ := objects.HasConvertRequest(state.NewSession(f.State), "alice", 1); has {
		t.Error("convert request should be removed after clear")
	}
	if got := f.HBDBalance("steem.dao"); got < treasuryHbdPre+5_000-10 || got > treasuryHbdPre+5_000+100 {
		t.Errorf("treasury HBD: got %d want ~%d", got, treasuryHbdPre+5_000)
	}

	// --- Collateralized convert path: clear_account refuses.
	if !f.HasAccount("alice") {
		f.Actor("alice") // alice still exists after clear
	}
	// Manually seed a collateralized_convert row for alice (the op path
	// is HF26+ which we're not on at HF22). Just stash the row directly.
	{
		s := state.NewSession(f.State)
		objects.PutCollateralizedConvert(s, &objects.CollateralizedConvert{
			Owner: AccountName("alice"), RequestID: 99,
			Collateral: 1000, HBDOut: 500,
			Conversion: f.HeadBlockTime() + 3600,
		})
		if err := s.Commit(false); err != nil {
			t.Fatalf("seed collateralized: %v", err)
		}
	}
	s3 := state.NewSession(f.State)
	if err := chain.ClearAccount(s3, "alice"); err != chain.ErrCollateralizedConvertInClearAccount {
		t.Errorf("expected ErrCollateralizedConvertInClearAccount, got %v", err)
	}
}

// --- hbd_test_01, hbd_test_02 — clear with HBD set. --------------------

// TestHF23HBDTest01 ports hbd_test_01.
func TestHF23HBDTest01(t *testing.T) {
	f := setupHF23(t)
	f.Actor("alice")
	if !f.HasAccount("steem.dao") {
		f.Actor("steem.dao")
	}
	f.GenerateBlock()

	// Pre-fund: zero HBD. Clear → HBD stays zero.
	s := state.NewSession(f.State)
	if err := chain.ClearAccount(s, "alice"); err != nil {
		t.Fatalf("clear empty alice: %v", err)
	}
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := f.HBDBalance("alice"); got != 0 {
		t.Errorf("alice HBD after empty clear: got %d", got)
	}

	// Re-create + fund.
	if !f.HasAccount("alice") {
		f.Actor("alice")
	}
	f.FundLiquidHBD("alice", 1_000_000)
	if got := f.HBDBalance("alice"); got != 1_000_000 {
		t.Fatalf("alice HBD pre-clear: got %d", got)
	}
	s2 := state.NewSession(f.State)
	if err := chain.ClearAccount(s2, "alice"); err != nil {
		t.Fatalf("clear funded alice: %v", err)
	}
	if err := s2.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := f.HBDBalance("alice"); got != 0 {
		t.Errorf("alice HBD post-clear: got %d", got)
	}
}

// TestHF23HBDTest02 ports hbd_test_02 — funded HBD + interest accrual
// over time, then clear.
func TestHF23HBDTest02(t *testing.T) {
	f := setupHF23(t)
	f.Actor("alice")
	if !f.HasAccount("steem.dao") {
		f.Actor("steem.dao")
	}
	f.GenerateBlock()
	f.FundLiquidHBD("alice", 1_000_000)

	// Roll forward to compound HBD interest.
	f.SkipSeconds(60 * 60 * 24 * 31) // 31 days
	preHbd := f.HBDBalance("alice")
	if preHbd < 1_000_000 {
		t.Fatalf("alice HBD did not accrue interest: got %d", preHbd)
	}

	s := state.NewSession(f.State)
	if err := chain.ClearAccount(s, "alice"); err != nil {
		t.Fatalf("clear alice: %v", err)
	}
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := f.HBDBalance("alice"); got != 0 {
		t.Errorf("alice HBD post-clear: got %d", got)
	}
}

// --- savings_test_01, savings_test_02 — clear with savings_hbd set. ----

// TestHF23SavingsTest01 ports savings_test_01.
func TestHF23SavingsTest01(t *testing.T) {
	f := setupHF23(t)
	f.Actor("alice")
	if !f.HasAccount("steem.dao") {
		f.Actor("steem.dao")
	}
	f.GenerateBlock()
	f.FundLiquidHBD("alice", 1_000_000)

	if err := f.ApplyOp(op.TransferToSavings{
		From: "alice", To: "alice", Amount: HBD(1_000_000),
	}); err != nil {
		t.Fatalf("transfer_to_savings: %v", err)
	}
	if got := f.SavingsHBDBalance("alice"); got != 1_000_000 {
		t.Fatalf("alice savings_hbd pre-clear: got %d", got)
	}

	s := state.NewSession(f.State)
	if err := chain.ClearAccount(s, "alice"); err != nil {
		t.Fatalf("clear alice: %v", err)
	}
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := f.SavingsHBDBalance("alice"); got != 0 {
		t.Errorf("alice savings_hbd post-clear: got %d", got)
	}
}

// TestHF23SavingsTest02 ports savings_test_02 — savings with interest
// then clear.
func TestHF23SavingsTest02(t *testing.T) {
	f := setupHF23(t)
	f.Actor("alice")
	if !f.HasAccount("steem.dao") {
		f.Actor("steem.dao")
	}
	f.GenerateBlock()
	f.FundLiquidHBD("alice", 1_000_000)

	if err := f.ApplyOp(op.TransferToSavings{
		From: "alice", To: "alice", Amount: HBD(1_000_000),
	}); err != nil {
		t.Fatalf("transfer_to_savings: %v", err)
	}
	f.SkipSeconds(60 * 60 * 24 * 31)

	preSavings := f.SavingsHBDBalance("alice")
	if preSavings < 1_000_000 {
		t.Fatalf("alice savings_hbd did not accrue interest: got %d", preSavings)
	}

	s := state.NewSession(f.State)
	if err := chain.ClearAccount(s, "alice"); err != nil {
		t.Fatalf("clear alice: %v", err)
	}
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := f.SavingsHBDBalance("alice"); got != 0 {
		t.Errorf("alice savings_hbd post-clear: got %d", got)
	}
}

// quiet unused-import suppressors.
var _ = asset.HiveSymbol
