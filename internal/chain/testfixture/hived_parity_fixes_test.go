package testfixture

import (
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// These tests pin evaluator rules found missing in the 2026-10 review
// against hived (docs/hived-parity-review-2026-10.md). Each one names the
// hived assertion it mirrors.

// B3 — hive_evaluator.cpp:498-501 and :505-508.
func TestPowRejectedAfterHF13AndPow2AfterHF17(t *testing.T) {
	f := New(t)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF13)
	if err := f.ApplyOp(op.Pow{WorkerAccount: "miner1", Props: validPowProps()}); err == nil {
		t.Fatal("pow accepted after HF13; hived: \"pow is deprecated. Use pow2 instead\"")
	}
	// pow2 is still legal between HF13 and HF17.
	if err := f.ApplyOp(op.Pow2{
		WorkVariantTag: 0,
		PowInput:       op.Pow2Input{WorkerAccount: "miner2"},
		Props:          validPowProps(),
	}); err != nil {
		t.Fatalf("pow2 between HF13 and HF17 rejected: %v", err)
	}
	f.HardforksUpTo(hardfork.HF17)
	if err := f.ApplyOp(op.Pow2{
		WorkVariantTag: 0,
		PowInput:       op.Pow2Input{WorkerAccount: "miner3"},
		Props:          validPowProps(),
	}); err == nil {
		t.Fatal("pow2 accepted after HF17; hived: \"mining is now disabled\"")
	}
	if has, _ := objects.HasAccount(state.NewSession(f.State), "miner3"); has {
		t.Fatal("rejected pow2 still created the worker account")
	}
}

// B6 — hive_evaluator_transfer.cpp:548-551 / :588-591.
func TestLimitOrderExpirationCapHF20(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.FundLiquid("alice", 100_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF20)
	f.GenerateBlock()

	const day = 24 * 60 * 60
	tooFar := f.HeadBlockTime() + 29*day
	if err := f.ApplyOp(op.LimitOrderCreate{
		Owner: "alice", OrderID: 1, AmountToSell: HIVE(1_000), MinToReceive: HBD(1_000), Expiration: tooFar,
	}); err == nil {
		t.Fatal("limit_order_create with a 29-day expiration accepted after HF20 (cap is 28 days)")
	}
	okExp := f.HeadBlockTime() + 28*day
	if err := f.ApplyOp(op.LimitOrderCreate{
		Owner: "alice", OrderID: 2, AmountToSell: HIVE(1_000), MinToReceive: HBD(1_000), Expiration: okExp,
	}); err != nil {
		t.Fatalf("limit_order_create at exactly head + 28 days rejected: %v", err)
	}
}

// B16 — hive_evaluator.cpp:218 and :254 "Proxy must change."
func TestAccountWitnessProxyMustChange(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()

	if err := f.ApplyOp(op.AccountWitnessProxy{Account: "alice", Proxy: ""}); err == nil {
		t.Fatal("clearing a proxy that is not set was accepted")
	}
	if err := f.ApplyOp(op.AccountWitnessProxy{Account: "alice", Proxy: "bob"}); err != nil {
		t.Fatalf("setting proxy: %v", err)
	}
	if err := f.ApplyOp(op.AccountWitnessProxy{Account: "alice", Proxy: "bob"}); err == nil {
		t.Fatal("setting the proxy that is already set was accepted")
	}
	if err := f.ApplyOp(op.AccountWitnessProxy{Account: "alice", Proxy: ""}); err != nil {
		t.Fatalf("clearing a set proxy: %v", err)
	}
}

// B7 — hive_evaluator_transfer.cpp:387-389 (HIVE_HARDFORK_1_28_FIX_CANCEL_POWER_DOWN,
// non-zero path): a power down at the rate already in force is rejected.
func TestWithdrawVestingSameRateRejectedHF28(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.FundVesting("alice", 1_000_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF28)
	f.GenerateBlock()

	a, err := objects.GetAccount(state.NewSession(f.State), "alice")
	if err != nil {
		t.Fatal(err)
	}
	half := a.VestingShares / 2
	if err := f.ApplyOp(op.WithdrawVesting{Account: "alice", VestingShares: VESTS(half)}); err != nil {
		t.Fatalf("first withdraw_vesting: %v", err)
	}
	if err := f.ApplyOp(op.WithdrawVesting{Account: "alice", VestingShares: VESTS(half)}); err == nil {
		t.Fatal("repeating the same power down was accepted; hived: \"This operation would not change the vesting withdraw rate.\"")
	}
	if err := f.ApplyOp(op.WithdrawVesting{Account: "alice", VestingShares: VESTS(half / 2)}); err != nil {
		t.Fatalf("power down at a different rate rejected: %v", err)
	}
}

// B12 — hive_evaluator.cpp:597-605: from HF20 a feed must be HBD base / HIVE quote.
func TestFeedPublishOrientationHF20(t *testing.T) {
	f := New(t)
	f.Actor("alice_witness")
	f.FundLiquid("alice_witness", 100_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF20)
	f.GenerateBlock()
	if err := f.ApplyOp(op.WitnessUpdate{
		Owner: "alice_witness", URL: "x",
		BlockSigningKey: [33]byte{0x02, 1},
		Props: op.LegacyChainProperties{
			AccountCreationFee: LegacyHIVE(1_000),
			MaximumBlockSize:   65536,
			HBDInterestRate:    1000,
		},
	}); err != nil {
		t.Fatalf("witness_update: %v", err)
	}
	reversed := asset.Price{Base: HIVE(1_000), Quote: HBD(500)}
	if err := f.ApplyOp(op.FeedPublish{Publisher: "alice_witness", ExchangeRate: reversed}); err == nil {
		t.Fatal("reversed (HIVE/HBD) feed accepted after HF20")
	}
	correct := asset.Price{Base: HBD(500), Quote: HIVE(1_000)}
	if err := f.ApplyOp(op.FeedPublish{Publisher: "alice_witness", ExchangeRate: correct}); err != nil {
		t.Fatalf("HBD/HIVE feed rejected: %v", err)
	}
}

// A10 — hive_evaluator_social.cpp:123-124: the 8-beneficiary cap is a soft
// fork (`!_db.is_in_control() || size <= 8`). The fixture applies ops in
// control, so 9 must be rejected here; the block-application path (not in
// control) must accept it — pinned on the evaluator directly below.
func TestCommentOptionsBeneficiaryCapIsSoft(t *testing.T) {
	f := New(t)
	f.Actors("alice", "b1", "b2", "b3", "b4", "b5", "b6", "b7", "b8", "b9")
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF21)
	f.GenerateBlock()
	f.MustApplyOp(op.Comment{
		Author: "alice", Permlink: "p", ParentPermlink: "cat", Title: "t", Body: "b", JSONMetadata: "{}",
	})
	var bens []op.CommentBeneficiary
	for _, n := range []string{"b1", "b2", "b3", "b4", "b5", "b6", "b7", "b8", "b9"} {
		bens = append(bens, op.CommentBeneficiary{Account: n, Weight: 100})
	}
	co := op.CommentOptions{
		Author: "alice", Permlink: "p",
		MaxAcceptedPayout: HBD(1_000_000_000), PercentHbd: 10000,
		AllowVotes: true, AllowCurationRewards: true,
		Beneficiaries: bens,
	}
	if err := f.ApplyOp(co); err == nil {
		t.Fatal("9 beneficiaries accepted while in control")
	}
	if err := f.ApplyOpNotInControl(co); err != nil {
		t.Fatalf("9 beneficiaries rejected while applying a block (soft fork must not fire): %v", err)
	}
}
