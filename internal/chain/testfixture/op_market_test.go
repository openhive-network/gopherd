package testfixture

import (
	"testing"

	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Port of operation_tests.cpp::convert_apply.
//
// alice/bob each have liquid HIVE + HBD; convert tests:
//   - failure when account lacks the required HBD
//   - failure when the requestid is duplicated
//   - success: alice's HBD goes into a pending convert_request_object
//
// Post-HF16 the conversion delay is 3.5 days, pre-HF16 7 days. We
// fund directly via FundLiquid/FundLiquidHBD which bypasses the
// transfer_to_savings flow.
func TestConvertApply(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 7_500)
	f.FundLiquid("bob", 3_000)
	f.FundLiquidHBD("alice", 2_500)
	f.FundLiquidHBD("bob", 7_000)
	f.GenerateBlock()

	// HBD balances after the GenerateBlock — one block of HBD interest
	// accrual is irrelevant here because the threshold is 30 days.
	if got := f.HBDBalance("alice"); got != 2_500 {
		t.Fatalf("alice HBD: got %d, want 2500", got)
	}

	t.Log("--- Test failure when account does not have the required HBD")
	if err := f.ApplyOp(op.Convert{
		Owner:     "alice",
		RequestID: 1,
		Amount:    HBD(5_000), // alice has 2.5
	}); err == nil {
		t.Fatal("expected insufficient-HBD failure")
	}
	if got := f.HBDBalance("alice"); got != 2_500 {
		t.Fatalf("alice HBD after failed convert: got %d, want 2500", got)
	}

	t.Log("--- Test failure when account does not exist")
	if err := f.ApplyOp(op.Convert{
		Owner:     "nobody",
		RequestID: 1,
		Amount:    HBD(1_000),
	}); err == nil {
		t.Fatal("expected account-not-found failure")
	}

	t.Log("--- Test success converting HBD to HIVE")
	if err := f.ApplyOp(op.Convert{
		Owner:     "bob",
		RequestID: 1,
		Amount:    HBD(3_000),
	}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got := f.HBDBalance("bob"); got != 4_000 {
		t.Fatalf("bob HBD after convert: got %d, want 4000", got)
	}
	// Pending request exists.
	s := state.NewSession(f.State)
	has, _ := objects.HasConvertRequest(s, "bob", 1)
	if !has {
		t.Fatal("convert_request bob/1 should exist")
	}

	t.Log("--- Test failure from repeated id")
	if err := f.ApplyOp(op.Convert{
		Owner:     "bob",
		RequestID: 1,
		Amount:    HBD(2_000),
	}); err == nil {
		t.Fatal("expected duplicate-id failure")
	}
	// State unchanged.
	if got := f.HBDBalance("bob"); got != 4_000 {
		t.Fatalf("bob HBD after duplicate: got %d, want 4000", got)
	}
}

// Port of operation_tests.cpp::limit_order_create_apply.
//
// Tests:
//   - failure when account lacks funds
//   - failure when amount-to-receive is 0
//   - failure when amount-to-sell is 0
//   - success creating an order that doesn't immediately fill
//   - failure when re-using the same order ID
//   - fill_or_kill failure on no-match
//   - partial match: alice's resting 10 HIVE at 1.5x → bob fills 5 HIVE
//     at the rate, leaving 5 HIVE on the book.
func TestLimitOrderCreateApply(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 1_000_000)    // 1000 HIVE
	f.FundLiquidHBD("bob", 1_000_000)   // 1000 HBD
	f.GenerateBlock()

	expr := f.HeadBlockTime() + 60*60 // 1h expiration

	t.Log("--- Test failure when account does not have required funds (bob lacks HIVE)")
	if err := f.ApplyOp(op.LimitOrderCreate{
		Owner:        "bob",
		OrderID:      1,
		AmountToSell: HIVE(10_000), // bob has 0 HIVE
		MinToReceive: HBD(10_000),
		FillOrKill:   false,
		Expiration:   expr,
	}); err == nil {
		t.Fatal("expected insufficient-balance")
	}
	if has, _ := objects.HasLimitOrder(state.NewSession(f.State), "bob", 1); has {
		t.Fatal("bob/1 order should not exist")
	}

	t.Log("--- Test failure when amount_to_receive is 0")
	if err := f.ApplyOp(op.LimitOrderCreate{
		Owner:        "alice",
		OrderID:      1,
		AmountToSell: HIVE(10_000),
		MinToReceive: HBD(0),
		FillOrKill:   false,
		Expiration:   expr,
	}); err == nil {
		t.Fatal("expected zero-min_to_receive failure")
	}

	t.Log("--- Test failure when amount_to_sell is 0")
	if err := f.ApplyOp(op.LimitOrderCreate{
		Owner:        "alice",
		OrderID:      1,
		AmountToSell: HIVE(0),
		MinToReceive: HBD(10_000),
		FillOrKill:   false,
		Expiration:   expr,
	}); err == nil {
		t.Fatal("expected zero-amount_to_sell failure")
	}

	t.Log("--- Test success creating limit order that won't be filled")
	// alice sells 10 HIVE, wants 15 HBD (price = 1 HIVE for 1.5 HBD).
	if err := f.ApplyOp(op.LimitOrderCreate{
		Owner:        "alice",
		OrderID:      1,
		AmountToSell: HIVE(10_000),
		MinToReceive: HBD(15_000),
		FillOrKill:   false,
		Expiration:   expr,
	}); err != nil {
		t.Fatalf("alice order: %v", err)
	}
	if has, _ := objects.HasLimitOrder(state.NewSession(f.State), "alice", 1); !has {
		t.Fatal("alice/1 order should exist")
	}
	if got := f.Balance("alice"); got != 990_000 {
		t.Fatalf("alice HIVE after order: got %d, want 990000", got)
	}

	t.Log("--- Test failure creating limit order with duplicate id")
	if err := f.ApplyOp(op.LimitOrderCreate{
		Owner:        "alice",
		OrderID:      1,
		AmountToSell: HIVE(20_000),
		MinToReceive: HBD(30_000),
		FillOrKill:   false,
		Expiration:   expr,
	}); err == nil {
		t.Fatal("expected duplicate-orderid failure")
	}

	t.Log("--- Test fill_or_kill failure when order won't fully fill")
	if err := f.ApplyOp(op.LimitOrderCreate{
		Owner:        "alice",
		OrderID:      2,
		AmountToSell: HIVE(10_000),
		MinToReceive: HBD(15_000),
		FillOrKill:   true,
		Expiration:   expr,
	}); err == nil {
		t.Fatal("expected fill_or_kill failure")
	}
	if has, _ := objects.HasLimitOrder(state.NewSession(f.State), "alice", 2); has {
		t.Fatal("alice/2 should not exist after FOK failure")
	}

	t.Log("--- Test partial match: bob sells 7.5 HBD for 5 HIVE; matches alice")
	if err := f.ApplyOp(op.LimitOrderCreate{
		Owner:        "bob",
		OrderID:      1,
		AmountToSell: HBD(7_500),
		MinToReceive: HIVE(5_000),
		FillOrKill:   false,
		Expiration:   expr,
	}); err != nil {
		t.Fatalf("bob fill order: %v", err)
	}
	// Bob's order fully fills (5 HIVE received, 7.5 HBD paid).
	// Alice's resting order had 10 HIVE for_sale → after fill 5 HIVE remain.
	s := state.NewSession(f.State)
	aliceOrder, err := objects.GetLimitOrder(s, "alice", 1)
	if err != nil {
		t.Fatalf("alice order should still exist: %v", err)
	}
	if aliceOrder.ForSaleAmount != 5_000 {
		t.Fatalf("alice for_sale: got %d, want 5000", aliceOrder.ForSaleAmount)
	}
	// Bob's order is gone (fully filled).
	if has, _ := objects.HasLimitOrder(s, "bob", 1); has {
		t.Fatal("bob/1 should be removed (fully filled)")
	}
	// Final balances.
	if got := f.Balance("alice"); got != 990_000 {
		t.Fatalf("alice HIVE after partial: got %d, want 990000", got)
	}
	if got := f.HBDBalance("alice"); got != 7_500 {
		t.Fatalf("alice HBD after partial: got %d, want 7500", got)
	}
	if got := f.Balance("bob"); got != 5_000 {
		t.Fatalf("bob HIVE after partial: got %d, want 5000", got)
	}
	if got := f.HBDBalance("bob"); got != 992_500 {
		t.Fatalf("bob HBD after partial: got %d, want 992500", got)
	}
}
