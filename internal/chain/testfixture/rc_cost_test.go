package testfixture

import (
	"testing"

	"hived-go/internal/chain"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
)

// ComputeRCCost should return zero pre-HF20 (RC subsystem inactive).
func TestComputeRCCostPreHF20Zero(t *testing.T) {
	signedTx := tx.SignedTransaction{
		Transaction: tx.Transaction{
			Operations: op.Ops(
				op.Transfer{From: "alice", To: "bob", Amount: HIVE(10)},
			),
		},
	}
	got := chain.ComputeRCCost(&signedTx, 1) // pre-HF20 block
	if got != 0 {
		t.Fatalf("pre-HF20 cost: got %d, want 0", got)
	}
}

// Different op types yield different costs (sanity: comment > transfer).
func TestComputeRCCostOrdering(t *testing.T) {
	const blockNum = 70_000_000 // post-HF20
	transferTx := tx.SignedTransaction{
		Transaction: tx.Transaction{
			Operations: op.Ops(
				op.Transfer{From: "alice", To: "bob", Amount: HIVE(10)},
			),
		},
	}
	commentTx := tx.SignedTransaction{
		Transaction: tx.Transaction{
			Operations: op.Ops(
				op.Comment{Author: "alice", Permlink: "p", Body: "hi"},
			),
		},
	}
	t1 := chain.ComputeRCCost(&transferTx, blockNum)
	t2 := chain.ComputeRCCost(&commentTx, blockNum)
	if t1 == 0 || t2 == 0 {
		t.Fatalf("post-HF20 costs should be non-zero: transfer=%d, comment=%d", t1, t2)
	}
	// Comment creates state (a row with an expensive lifetime); transfer
	// is just exec time. Comment must cost more.
	if t2 <= t1 {
		t.Fatalf("comment cost (%d) should exceed transfer cost (%d)", t2, t1)
	}
}

// limit_order_create with FillOrKill should cost LESS than persistent
// orders (no state row created).
func TestComputeRCCostFillOrKillCheaper(t *testing.T) {
	const blockNum = 70_000_000
	hbd := HBD(1)
	hive := HIVE(1)
	persistTx := tx.SignedTransaction{
		Transaction: tx.Transaction{
			Operations: op.Ops(
				op.LimitOrderCreate{
					Owner: "alice", OrderID: 1,
					AmountToSell: hbd, MinToReceive: hive,
					FillOrKill: false,
				},
			),
		},
	}
	fokTx := tx.SignedTransaction{
		Transaction: tx.Transaction{
			Operations: op.Ops(
				op.LimitOrderCreate{
					Owner: "alice", OrderID: 1,
					AmountToSell: hbd, MinToReceive: hive,
					FillOrKill: true,
				},
			),
		},
	}
	cPersist := chain.ComputeRCCost(&persistTx, blockNum)
	cFOK := chain.ComputeRCCost(&fokTx, blockNum)
	if cPersist <= cFOK {
		t.Fatalf("persistent order should cost more than fill-or-kill: persist=%d fok=%d",
			cPersist, cFOK)
	}
}
