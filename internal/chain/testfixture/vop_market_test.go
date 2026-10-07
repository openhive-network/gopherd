package testfixture

import (
	"testing"

	"gopherd/internal/chain"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/op"
)

// TestVOpFillOrderAndCancel pins two of the market virtual ops that were
// defined but never emitted: fill_order (database_market.cpp:342, one per
// match, incoming order first) and limit_order_cancelled (database_market.cpp:81).
func TestVOpFillOrderAndCancel(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 100_000)
	f.FundLiquidHBD("bob", 100_000)
	f.GenerateBlock()

	exp := f.HeadBlockTime() + 60*60
	f.MustApplyOp(op.LimitOrderCreate{
		Owner: "alice", OrderID: 1, AmountToSell: HIVE(10_000), MinToReceive: HBD(5_000), Expiration: exp,
	})
	// bob takes alice's order at her price.
	f.MustApplyOp(op.LimitOrderCreate{
		Owner: "bob", OrderID: 7, AmountToSell: HBD(5_000), MinToReceive: HIVE(10_000), Expiration: exp,
	})
	vops := f.Chain.LastBlockVOps()
	v := chain.FindVOp(vops, op.TagFillOrder)
	if v == nil {
		t.Fatalf("no fill_order in the block's vops: %v", vops)
	}
	fo := v.(op.FillOrder)
	if fo.CurrentOwner != "bob" || fo.CurrentOrderID != 7 || fo.OpenOwner != "alice" || fo.OpenOrderID != 1 {
		t.Fatalf("fill_order sides: got current=%s/%d open=%s/%d, want current=bob/7 open=alice/1",
			fo.CurrentOwner, fo.CurrentOrderID, fo.OpenOwner, fo.OpenOrderID)
	}
	if fo.CurrentPays.Symbol.Num != asset.HbdSymbol || fo.CurrentPays.Amount != 5_000 {
		t.Fatalf("current_pays: got %v, want 5000 HBD", fo.CurrentPays)
	}
	if fo.OpenPays.Symbol.Num != asset.HiveSymbol || fo.OpenPays.Amount != 10_000 {
		t.Fatalf("open_pays: got %v, want 10000 HIVE", fo.OpenPays)
	}

	// An explicit cancel reports the refund in the order's sell asset.
	f.MustApplyOp(op.LimitOrderCreate{
		Owner: "alice", OrderID: 2, AmountToSell: HIVE(3_000), MinToReceive: HBD(9_000), Expiration: exp,
	})
	f.MustApplyOp(op.LimitOrderCancel{Owner: "alice", OrderID: 2})
	v = chain.FindVOp(f.Chain.LastBlockVOps(), op.TagLimitOrderCancelled)
	if v == nil {
		t.Fatal("no limit_order_cancelled after limit_order_cancel")
	}
	lc := v.(op.LimitOrderCancelled)
	if lc.Seller != "alice" || lc.OrderID != 2 || lc.AmountBack.Amount != 3_000 || lc.AmountBack.Symbol.Num != asset.HiveSymbol {
		t.Fatalf("limit_order_cancelled: got %+v", lc)
	}
}

// TestVOpAccountWitnessProxyAndRecovery pins changed_recovery_account
// (database.cpp:2025): emitted when the 30-day change request matures, with
// the previous agent's name.
func TestVOpChangedRecoveryAccount(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob", "carol")
	f.GenerateBlock()
	f.MustApplyOp(op.ChangeRecoveryAccount{AccountToRecover: "alice", NewRecoveryAccount: "carol"})
	f.SkipSeconds(30*24*60*60 + 60)
	v := chain.FindVOp(f.Chain.LastBlockVOps(), op.TagChangedRecoveryAccount)
	if v == nil {
		t.Fatal("no changed_recovery_account when the request matured")
	}
	cr := v.(op.ChangedRecoveryAccount)
	if cr.Account != "alice" || cr.NewRecoveryAccount != "carol" {
		t.Fatalf("changed_recovery_account: got %+v", cr)
	}
}
