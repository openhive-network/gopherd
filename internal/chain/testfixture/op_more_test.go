package testfixture

import (
	"testing"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// Port of operation_tests.cpp::feed_publish_apply.
// Witness publishes a feed; the witness's HBDExchangeRate updates.
func TestFeedPublishApply(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.FundLiquid("alice", 10_000)
	f.GenerateBlock()

	// Make alice a witness first.
	if err := f.ApplyOp(op.WitnessUpdate{
		Owner:           "alice",
		URL:             "x",
		BlockSigningKey: [33]byte{0x02, 1},
		Props: op.LegacyChainProperties{
			AccountCreationFee: LegacyHIVE(1_000),
			MaximumBlockSize:   65536,
			HBDInterestRate:    1000,
		},
	}); err != nil {
		t.Fatalf("witness_update: %v", err)
	}

	t.Log("--- Publish a feed")
	feed := asset.Price{
		Base:  asset.Asset{Amount: 100, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
		Quote: asset.Asset{Amount: 100, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
	}
	if err := f.ApplyOp(op.FeedPublish{
		Publisher:    "alice",
		ExchangeRate: feed,
	}); err != nil {
		t.Fatalf("feed_publish: %v", err)
	}
	w := f.Witness("alice")
	if w.HBDExchangeRate.Base.Amount != 100 {
		t.Fatalf("witness.HBDExchangeRate.Base: got %d, want 100", w.HBDExchangeRate.Base.Amount)
	}

	t.Log("--- Failure when publisher isn't a witness")
	if err := f.ApplyOp(op.FeedPublish{
		Publisher:    "nobody",
		ExchangeRate: feed,
	}); err == nil {
		t.Fatal("expected unknown-publisher failure")
	}
}

// Port of operation_tests.cpp::limit_order_cancel_apply.
func TestLimitOrderCancelApply(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.FundLiquid("alice", 10_000)
	f.GenerateBlock()

	expr := f.HeadBlockTime() + 60*60
	if err := f.ApplyOp(op.LimitOrderCreate{
		Owner:        "alice",
		OrderID:      1,
		AmountToSell: HIVE(5_000),
		MinToReceive: HBD(7_500),
		Expiration:   expr,
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if got := f.Balance("alice"); got != 5_000 {
		t.Fatalf("alice HIVE after order: got %d, want 5000", got)
	}

	t.Log("--- Cancel returns funds")
	if err := f.ApplyOp(op.LimitOrderCancel{
		Owner:   "alice",
		OrderID: 1,
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := f.Balance("alice"); got != 10_000 {
		t.Fatalf("alice HIVE after cancel: got %d, want 10000", got)
	}

	t.Log("--- Failure cancelling non-existent order")
	if err := f.ApplyOp(op.LimitOrderCancel{
		Owner:   "alice",
		OrderID: 99,
	}); err == nil {
		t.Fatal("expected order-not-found failure")
	}
}

// Port of operation_tests.cpp::limit_order_create2_apply.
// limit_order_create2 differs from create in that the price is
// expressed as an explicit `exchange_rate` price object rather than
// (amount_to_sell, min_to_receive) pair.
func TestLimitOrderCreate2Apply(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.FundLiquid("alice", 10_000)
	f.GenerateBlock()

	expr := f.HeadBlockTime() + 60*60
	rate := asset.Price{
		Base:  asset.Asset{Amount: 1_000, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
		Quote: asset.Asset{Amount: 1_500, Symbol: asset.Symbol{Num: asset.HbdSymbol}},
	}
	if err := f.ApplyOp(op.LimitOrderCreate2{
		Owner:        "alice",
		OrderID:      1,
		AmountToSell: HIVE(5_000),
		ExchangeRate: rate,
		Expiration:   expr,
	}); err != nil {
		t.Fatalf("create2: %v", err)
	}
	if got := f.Balance("alice"); got != 5_000 {
		t.Fatalf("alice HIVE after order: got %d, want 5000", got)
	}
	// Stored sell_price preserves exchange_rate.base/quote verbatim
	// (mirrors C++ `_db.create<limit_order_object>(seller, amount_to_sell,
	// o.exchange_rate, ...)`). The implied receive at full fill is
	// for_sale × quote / base = 5000 × 1500 / 1000 = 7500 sat-HBD, but
	// that's a derived quantity, not a stored field.
	s := state.NewSession(f.State)
	o, err := objects.GetLimitOrder(s, "alice", 1)
	if err != nil {
		t.Fatalf("order lookup: %v", err)
	}
	if o.SellPriceBaseAmount != 1_000 {
		t.Fatalf("SellPriceBaseAmount: got %d, want 1000", o.SellPriceBaseAmount)
	}
	if o.MinReceiveAmt != 1_500 {
		t.Fatalf("MinReceiveAmt (sell_price.quote): got %d, want 1500", o.MinReceiveAmt)
	}
	if o.ForSaleAmount != 5_000 {
		t.Fatalf("ForSaleAmount: got %d, want 5000", o.ForSaleAmount)
	}
}

// Port of operation_tests.cpp::escrow_approve_apply (subset).
func TestEscrowApproveApply(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob", "sam")
	f.FundLiquid("alice", 10_000)
	f.GenerateBlock()

	// Set up an escrow.
	now := f.HeadBlockTime()
	if err := f.ApplyOp(op.EscrowTransfer{
		From:                 "alice",
		To:                   "bob",
		Agent:                "sam",
		EscrowID:             0,
		HiveAmount:           HIVE(1_000),
		HbdAmount:            HBD(0),
		Fee:                  HIVE(100),
		RatificationDeadline: now + 100,
		EscrowExpiration:     now + 200,
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	t.Log("--- bob approves")
	if err := f.ApplyOp(op.EscrowApprove{
		From:     "alice",
		To:       "bob",
		Agent:    "sam",
		Who:      "bob",
		EscrowID: 0,
		Approve:  true,
	}); err != nil {
		t.Fatalf("bob approve: %v", err)
	}

	s := state.NewSession(f.State)
	esc, err := objects.GetEscrow(s, "alice", 0)
	if err != nil {
		t.Fatalf("escrow lookup: %v", err)
	}
	if !esc.ToApproved {
		t.Fatal("ToApproved should be set")
	}
	if esc.AgentApproved {
		t.Fatal("AgentApproved should still be false")
	}

	t.Log("--- sam (agent) approves; fee released to sam")
	preSam := f.Balance("sam")
	if err := f.ApplyOp(op.EscrowApprove{
		From:     "alice",
		To:       "bob",
		Agent:    "sam",
		Who:      "sam",
		EscrowID: 0,
		Approve:  true,
	}); err != nil {
		t.Fatalf("sam approve: %v", err)
	}
	if got := f.Balance("sam") - preSam; got != 100 {
		t.Fatalf("sam fee credit: got %d, want 100", got)
	}

	t.Log("--- escrow now fully approved")
	s = state.NewSession(f.State)
	esc, _ = objects.GetEscrow(s, "alice", 0)
	if !esc.ToApproved || !esc.AgentApproved {
		t.Fatal("escrow should be fully approved")
	}
	if esc.PendingFee != 0 {
		t.Fatalf("PendingFee after agent approve: got %d, want 0", esc.PendingFee)
	}
}

// Port of operation_tests.cpp::escrow_dispute_apply (subset).
func TestEscrowDisputeApply(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob", "sam")
	f.FundLiquid("alice", 10_000)
	f.GenerateBlock()

	now := f.HeadBlockTime()
	if err := f.ApplyOp(op.EscrowTransfer{
		From: "alice", To: "bob", Agent: "sam",
		EscrowID: 0, HiveAmount: HIVE(1_000), HbdAmount: HBD(0), Fee: HIVE(100),
		RatificationDeadline: now + 100,
		EscrowExpiration:     now + 200,
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Both approve.
	if err := f.ApplyOp(op.EscrowApprove{
		From: "alice", To: "bob", Agent: "sam", Who: "bob",
		EscrowID: 0, Approve: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.ApplyOp(op.EscrowApprove{
		From: "alice", To: "bob", Agent: "sam", Who: "sam",
		EscrowID: 0, Approve: true,
	}); err != nil {
		t.Fatal(err)
	}

	t.Log("--- alice disputes")
	if err := f.ApplyOp(op.EscrowDispute{
		From: "alice", To: "bob", Agent: "sam", Who: "alice",
		EscrowID: 0,
	}); err != nil {
		t.Fatalf("dispute: %v", err)
	}
	s := state.NewSession(f.State)
	esc, _ := objects.GetEscrow(s, "alice", 0)
	if !esc.Disputed {
		t.Fatal("escrow should be disputed")
	}

	t.Log("--- Cannot dispute twice")
	if err := f.ApplyOp(op.EscrowDispute{
		From: "alice", To: "bob", Agent: "sam", Who: "alice",
		EscrowID: 0,
	}); err == nil {
		t.Fatal("expected already-disputed failure")
	}
}

// Port of operation_tests.cpp::escrow_release_apply (subset).
func TestEscrowReleaseApply(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob", "sam")
	f.FundLiquid("alice", 10_000)
	f.GenerateBlock()

	now := f.HeadBlockTime()
	if err := f.ApplyOp(op.EscrowTransfer{
		From: "alice", To: "bob", Agent: "sam",
		EscrowID: 0, HiveAmount: HIVE(1_000), HbdAmount: HBD(0), Fee: HIVE(100),
		RatificationDeadline: now + 100,
		EscrowExpiration:     now + 200,
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	for _, who := range []string{"bob", "sam"} {
		if err := f.ApplyOp(op.EscrowApprove{
			From: "alice", To: "bob", Agent: "sam", Who: who,
			EscrowID: 0, Approve: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Log("--- alice releases 0.500 HIVE to bob")
	preBob := f.Balance("bob")
	if err := f.ApplyOp(op.EscrowRelease{
		From: "alice", To: "bob", Agent: "sam", Who: "alice",
		Receiver: "bob", EscrowID: 0,
		HiveAmount: HIVE(500), HbdAmount: HBD(0),
	}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := f.Balance("bob") - preBob; got != 500 {
		t.Fatalf("bob credit: got %d, want 500", got)
	}
}

// Port of operation_tests.cpp::cancel_transfer_from_savings_apply.
func TestCancelTransferFromSavingsApply(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.FundLiquid("alice", 10_000)
	f.GenerateBlock()
	if err := f.ApplyOp(op.TransferToSavings{
		From: "alice", To: "alice", Amount: HIVE(5_000),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.ApplyOp(op.TransferFromSavings{
		From: "alice", RequestID: 1, To: "alice", Amount: HIVE(2_000),
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.SavingsBalance("alice"); got != 3_000 {
		t.Fatalf("savings after schedule: got %d, want 3000", got)
	}

	t.Log("--- Cancel returns funds to savings")
	if err := f.ApplyOp(op.CancelTransferFromSavings{
		From: "alice", RequestID: 1,
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := f.SavingsBalance("alice"); got != 5_000 {
		t.Fatalf("savings after cancel: got %d, want 5000", got)
	}

	t.Log("--- Failure cancelling missing request")
	if err := f.ApplyOp(op.CancelTransferFromSavings{
		From: "alice", RequestID: 99,
	}); err == nil {
		t.Fatal("expected request-not-found failure")
	}
}

// Port of operation_tests.cpp::claim_reward_balance_apply (subset).
func TestClaimRewardBalanceApply(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.GenerateBlock()
	// Seed alice's reward buckets directly (bypassing the cashout
	// pipeline that fills them under normal operation).
	{
		s := state.NewSession(f.State)
		a, _ := objects.GetAccount(s, "alice")
		a.RewardHive = 1_000
		a.RewardHBD = 500
		a.RewardVests = 200_000
		objects.PutAccount(s, &a)
		_ = s.Commit(false)
	}
	preBal := f.Balance("alice")
	preVests := f.Vests("alice")

	t.Log("--- claim_reward_balance moves rewards to liquid + vesting")
	if err := f.ApplyOp(op.ClaimRewardBalance{
		Account:     "alice",
		RewardHive:  HIVE(1_000),
		RewardHbd:   HBD(500),
		RewardVests: VESTS(200_000),
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got := f.Balance("alice") - preBal; got != 1_000 {
		t.Fatalf("alice HIVE delta: got %d, want 1000", got)
	}
	if got := f.HBDBalance("alice"); got != 500 {
		t.Fatalf("alice HBD: got %d, want 500", got)
	}
	if got := f.Vests("alice") - preVests; got != 200_000 {
		t.Fatalf("alice vests delta: got %d, want 200000", got)
	}
	a := f.Account_("alice")
	if a.RewardHive != 0 || a.RewardHBD != 0 || a.RewardVests != 0 {
		t.Fatalf("reward balances should be zero after claim: %+v",
			[3]int64{a.RewardHive, a.RewardHBD, a.RewardVests})
	}

	t.Log("--- Failure claiming more than available")
	if err := f.ApplyOp(op.ClaimRewardBalance{
		Account:    "alice",
		RewardHive: HIVE(100),
	}); err == nil {
		t.Fatal("expected insufficient-reward failure")
	}
}

// Port of operation_tests.cpp::recurrent_transfer_apply (HF25+).
func TestRecurrentTransferApply(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 100_000)
	f.GenerateBlock()
	f.HardforksUpTo(hardfork.HF25)

	t.Log("--- Create a recurrent transfer schedule")
	if err := f.ApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     HIVE(1_000),
		Memo:       "monthly",
		Recurrence: 24, // hours
		Executions: 3,
	}); err != nil {
		t.Fatalf("recurrent: %v", err)
	}
	has, _ := objects.HasRecurrentTransfer(state.NewSession(f.State), "alice", "bob", 0)
	if !has {
		t.Fatal("recurrent transfer should exist")
	}

	t.Log("--- Delete schedule with amount=0 (executions >= 2 pre-HF29)")
	if err := f.ApplyOp(op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Amount:     HIVE(0),
		Recurrence: 0,
		Executions: 2,
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	has, _ = objects.HasRecurrentTransfer(state.NewSession(f.State), "alice", "bob", 0)
	if has {
		t.Fatal("recurrent transfer should be deleted")
	}
}

// Port of operation_tests.cpp::comment_options_apply (subset).
func TestCommentOptionsApply(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.GenerateBlock()
	if err := f.ApplyOp(op.Comment{ParentPermlink: "test",
		Author: "alice", Permlink: "p", Body: "hi",
	}); err != nil {
		t.Fatalf("comment: %v", err)
	}

	t.Log("--- comment_options can lower max_accepted_payout + percent_hbd")
	if err := f.ApplyOp(op.CommentOptions{
		Author:               "alice",
		Permlink:             "p",
		MaxAcceptedPayout:    HBD(500_000_000),
		PercentHbd:           5000,
		AllowVotes:           true,
		AllowCurationRewards: true,
	}); err != nil {
		t.Fatalf("comment_options: %v", err)
	}
	c := f.Comment("alice", "p")
	if c.PercentHbd != 5000 {
		t.Fatalf("PercentHbd: got %d, want 5000", c.PercentHbd)
	}

	t.Log("--- Cannot raise max_accepted_payout")
	if err := f.ApplyOp(op.CommentOptions{
		Author:               "alice",
		Permlink:             "p",
		MaxAcceptedPayout:    HBD(999_999_999_999),
		PercentHbd:           5000,
		AllowVotes:           true,
		AllowCurationRewards: true,
	}); err == nil {
		t.Fatal("expected raising max_payout to fail")
	}

	t.Log("--- Cannot re-enable votes once disabled")
	// First, disable.
	if err := f.ApplyOp(op.CommentOptions{
		Author:               "alice",
		Permlink:             "p",
		MaxAcceptedPayout:    HBD(500_000_000),
		PercentHbd:           5000,
		AllowVotes:           false,
		AllowCurationRewards: true,
	}); err != nil {
		t.Fatalf("disable votes: %v", err)
	}
	if err := f.ApplyOp(op.CommentOptions{
		Author:               "alice",
		Permlink:             "p",
		MaxAcceptedPayout:    HBD(500_000_000),
		PercentHbd:           5000,
		AllowVotes:           true,
		AllowCurationRewards: true,
	}); err == nil {
		t.Fatal("expected re-enable-votes to fail")
	}
}
