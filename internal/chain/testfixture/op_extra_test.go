package testfixture

import (
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
)

// Port of operation_tests.cpp::comment_beneficiaries_apply (subset).
func TestCommentBeneficiariesApply(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF17)
	f.Actors("alice", "bob", "sam", "dave")
	f.GenerateBlock()

	if err := f.ApplyOp(op.Comment{
		Author: "alice", Permlink: "test", Body: "x",
	}); err != nil {
		t.Fatal(err)
	}

	t.Log("--- Failure on more than 8 beneficiaries")
	bf := []op.CommentBeneficiary{
		{Account: "bob", Weight: 100},
		{Account: "sam", Weight: 100},
		{Account: "dave", Weight: 100},
		{Account: "alice", Weight: 100},
		{Account: InitMiner, Weight: 100},
		{Account: "bob", Weight: 100},
		{Account: "sam", Weight: 100},
		{Account: "dave", Weight: 100},
		{Account: "alice", Weight: 100}, // 9 entries
	}
	if err := f.ApplyOp(op.CommentOptions{
		Author: "alice", Permlink: "test",
		MaxAcceptedPayout:    HBD(1_000_000_000),
		PercentHbd:           10000,
		AllowVotes:           true,
		AllowCurationRewards: true,
		Beneficiaries:        bf,
	}); err == nil {
		t.Fatal("expected too-many-beneficiaries failure")
	}

	t.Log("--- Failure on non-existent beneficiary")
	if err := f.ApplyOp(op.CommentOptions{
		Author: "alice", Permlink: "test",
		MaxAcceptedPayout:    HBD(1_000_000_000),
		PercentHbd:           10000,
		AllowVotes:           true,
		AllowCurationRewards: true,
		Beneficiaries:        []op.CommentBeneficiary{{Account: "ghost", Weight: 100}},
	}); err == nil {
		t.Fatal("expected unknown-account failure")
	}

	t.Log("--- Success: bob gets 25%, sam gets 50%, dave gets 10%")
	if err := f.ApplyOp(op.CommentOptions{
		Author: "alice", Permlink: "test",
		MaxAcceptedPayout:    HBD(1_000_000_000),
		PercentHbd:           10000,
		AllowVotes:           true,
		AllowCurationRewards: true,
		Beneficiaries: []op.CommentBeneficiary{
			{Account: "bob", Weight: 2500},
			{Account: "dave", Weight: 1000},
			{Account: "sam", Weight: 5000},
		},
	}); err != nil {
		t.Fatalf("set beneficiaries: %v", err)
	}
	c := f.Comment("alice", "test")
	if len(c.Beneficiaries) != 3 {
		t.Fatalf("Beneficiaries len: got %d, want 3", len(c.Beneficiaries))
	}

	t.Log("--- Cannot set beneficiaries twice")
	if err := f.ApplyOp(op.CommentOptions{
		Author: "alice", Permlink: "test",
		MaxAcceptedPayout:    HBD(1_000_000_000),
		PercentHbd:           10000,
		AllowVotes:           true,
		AllowCurationRewards: true,
		Beneficiaries:        []op.CommentBeneficiary{{Account: "bob", Weight: 100}},
	}); err == nil {
		t.Fatal("expected already-set failure")
	}
}

// Port of operation_tests.cpp::witness_set_properties_apply (HF20+).
func TestWitnessSetPropertiesApply(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF20)
	f.Actor("alice")
	f.FundLiquid("alice", 100_000)
	f.GenerateBlock()

	signKey := [33]byte{0x02, 'A', 1}
	if err := f.ApplyOp(op.WitnessUpdate{
		Owner: "alice", URL: "x", BlockSigningKey: signKey,
		Props: op.LegacyChainProperties{
			AccountCreationFee: LegacyHIVE(1_000),
			MaximumBlockSize:   65536,
			HBDInterestRate:    1000,
		},
	}); err != nil {
		t.Fatalf("witness_update: %v", err)
	}

	t.Log("--- witness_set_properties updates URL")
	urlBytes := []byte("https://updated.url")
	if err := f.ApplyOp(op.WitnessSetProperties{
		Owner: "alice",
		Props: []op.WitnessProperty{
			{Key: "key", Value: append([]byte{}, signKey[:]...)},
			{Key: "url", Value: urlBytes},
		},
	}); err != nil {
		t.Fatalf("set_props: %v", err)
	}
	if got := f.Witness("alice").URL; got != "https://updated.url" {
		t.Fatalf("URL: got %q", got)
	}
}

// Port of operation_tests.cpp::set_withdraw_vesting_route_apply.
func TestSetWithdrawVestingRouteApply(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundVesting("alice", 100_000)
	f.GenerateBlock()

	t.Log("--- Set withdraw route from alice → bob 50% as VESTS")
	if err := f.ApplyOp(op.SetWithdrawVestingRoute{
		FromAccount: "alice",
		ToAccount:   "bob",
		Percent:     5000,
		AutoVest:    true,
	}); err != nil {
		t.Fatalf("set route: %v", err)
	}

	t.Log("--- Failure setting route to non-existent account")
	if err := f.ApplyOp(op.SetWithdrawVestingRoute{
		FromAccount: "alice",
		ToAccount:   "ghost",
		Percent:     1000,
		AutoVest:    true,
	}); err == nil {
		t.Fatal("expected unknown-to failure")
	}
}

// Port of operation_time_tests.cpp::vesting_withdraw_route.
//
// alice powers down, then routes 50% of each withdraw chunk to bob as
// VESTS (auto_vest=true) and 30% to sam as HIVE (auto_vest=false). The
// remaining 20% stays with alice as HIVE. After one weekly chunk:
//   - alice.vesting_shares decreases by the per-chunk rate
//   - alice.hive_balance += 20% × rate priced through dgpo
//   - bob.vesting_shares += 50% × rate (auto_vest=true preserves VESTS)
//   - sam.hive_balance += 30% × rate priced through dgpo
func TestVestingWithdrawRoute(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF16) // post-HF16 → 13-week schedule
	f.Actors("alice", "bob", "sam")
	f.GenerateBlock()
	f.FundVesting("alice", 1_040_000)
	f.GenerateBlock()

	startVests := f.Vests("alice")
	if startVests == 0 {
		t.Fatal("alice should have VESTS after FundVesting")
	}

	// Power down all of alice's VESTS.
	if err := f.ApplyOp(op.WithdrawVesting{
		Account:       "alice",
		VestingShares: VESTS(startVests),
	}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	a := f.Account_("alice")
	rate := a.VestingWithdrawRate
	if rate <= 0 {
		t.Fatalf("VestingWithdrawRate should be set, got %d", rate)
	}

	// Two routes: bob 50% auto_vest, sam 30% liquid.
	if err := f.ApplyOp(op.SetWithdrawVestingRoute{
		FromAccount: "alice", ToAccount: "bob", Percent: 5000, AutoVest: true,
	}); err != nil {
		t.Fatalf("route bob: %v", err)
	}
	if err := f.ApplyOp(op.SetWithdrawVestingRoute{
		FromAccount: "alice", ToAccount: "sam", Percent: 3000, AutoVest: false,
	}); err != nil {
		t.Fatalf("route sam: %v", err)
	}

	oldAliceVests := f.Vests("alice")
	oldAliceBalance := f.Balance("alice")
	oldBobVests := f.Vests("bob")
	oldSamBalance := f.Balance("sam")

	// Skip to the first withdrawal date.
	f.SkipSeconds(60*60*24*7 + 60)

	// alice.vesting_shares should drop by exactly rate (the chunk).
	if got := oldAliceVests - f.Vests("alice"); got != rate {
		t.Fatalf("alice VESTS delta: got %d, want %d", got, rate)
	}
	// bob.vesting_shares should rise by 50% of rate.
	bobDelta := f.Vests("bob") - oldBobVests
	want := rate * 5000 / 10000
	if bobDelta != want {
		t.Fatalf("bob VESTS delta: got %d, want %d (50%% of rate)", bobDelta, want)
	}
	// sam.balance should rise (30% × rate → priced HIVE).
	samDelta := f.Balance("sam") - oldSamBalance
	if samDelta <= 0 {
		t.Fatalf("sam HIVE delta: got %d, want > 0", samDelta)
	}
	// alice.balance should rise (20% × rate → priced HIVE).
	aliceDelta := f.Balance("alice") - oldAliceBalance
	if aliceDelta <= 0 {
		t.Fatalf("alice HIVE delta: got %d, want > 0 (20%% to self)", aliceDelta)
	}
	// And sam should get the bigger share (30% > 20%).
	if samDelta <= aliceDelta {
		t.Fatalf("sam HIVE %d should exceed alice HIVE %d (30%% > 20%%)",
			samDelta, aliceDelta)
	}
}

// Port of operation_tests.cpp::collateralized_convert_apply (HF25+, subset).
func TestCollateralizedConvertApply(t *testing.T) {
	f := New(t)
	f.HardforksUpTo(hardfork.HF25)
	f.Actor("alice")
	f.FundLiquid("alice", 100_000)
	f.GenerateBlock()

	// HF25 evaluator requires an active feed + non-zero print-rate.
	// Inject a 1 HBD = 1 HIVE feed directly + force print_rate>0 so
	// the test focuses on the collateral-debit path.
	setCollateralizedFeedAndPrintRate(t, f,
		asset.Price{
			Base:  asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
			Quote: asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
		})

	t.Log("--- collateralized_convert debits HIVE collateral")
	if err := f.ApplyOp(op.CollateralizedConvert{
		Owner: "alice", RequestID: 1, Amount: HIVE(10_000),
	}); err != nil {
		t.Fatalf("collateralized_convert: %v", err)
	}
	if got := f.Balance("alice"); got != 90_000 {
		t.Fatalf("alice HIVE after CC: got %d, want 90000", got)
	}
}

// Port of operation_tests.cpp::custom_apply (we treat as no-op).
func TestCustomOpNoop(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.GenerateBlock()
	if err := f.ApplyOp(op.Custom{
		RequiredAuths: []string{"alice"},
		ID:            42,
		Data:          []byte("payload"),
	}); err != nil {
		t.Fatalf("custom: %v", err)
	}
	if err := f.ApplyOp(op.CustomJSON{
		RequiredPostingAuths: []string{"alice"},
		ID:                   "test",
		JSON:                 `{"hi":1}`,
	}); err != nil {
		t.Fatalf("custom_json: %v", err)
	}
}
