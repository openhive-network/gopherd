package testfixture

import (
	"strings"
	"testing"

	"gopherd/internal/chain/evaluator"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// TestEscrowLimit ports a slice of operation_tests.cpp::escrow_limit:
// alice opens HIVE_MAX_PENDING_TRANSFERS=255 fresh escrow transfers,
// and the next one fails with the limit assertion. We use a single
// block (no per-block generate loop) since the chain-state assert is
// what matters here, not the per-block ratification-deadline machinery.
//
// The full C++ test additionally exercises approve/decline/expire/
// dispute/release transitions in a long mixed sequence; those flows
// have separate coverage in op_escrow_test.go.
func TestEscrowLimit(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob", "sam")
	f.FundLiquid("alice", 10_000_000) // plenty to cover 256 × 0.001 HIVE
	f.GenerateBlock()

	// Open MaxPendingTransfers fresh escrows from alice. Each block
	// advances head_block_time by HIVE_BLOCK_INTERVAL seconds, so we
	// must refresh the deadline relative to current head_block_time on
	// every iteration (255 blocks × 3 s ≈ 13 min of in-fixture time).
	for i := 0; i < evaluator.MaxPendingTransfers; i++ {
		now := f.HeadBlockTime()
		if err := f.ApplyOp(op.EscrowTransfer{
			From:                 "alice",
			To:                   "bob",
			Agent:                "sam",
			EscrowID:             uint32(i),
			HiveAmount:           HIVE(1),
			HbdAmount:            HBD(0),
			Fee:                  HIVE(0),
			RatificationDeadline: now + 24*60*60, // 24 h — well past every loop tick
			EscrowExpiration:     now + 48*60*60,
		}); err != nil {
			t.Fatalf("escrow_transfer #%d: %v", i, err)
		}
	}

	// Verify the count via the helper.
	s := state.NewSession(f.State)
	n, err := objects.CountEscrowsByFrom(s, auth.AccountName("alice"))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != evaluator.MaxPendingTransfers {
		t.Fatalf("escrow count: got %d, want %d", n, evaluator.MaxPendingTransfers)
	}

	// 256th must fail.
	now := f.HeadBlockTime()
	err = f.ApplyOp(op.EscrowTransfer{
		From:                 "alice",
		To:                   "bob",
		Agent:                "sam",
		EscrowID:             uint32(evaluator.MaxPendingTransfers),
		HiveAmount:           HIVE(1),
		HbdAmount:            HBD(0),
		Fee:                  HIVE(0),
		RatificationDeadline: now + 24*60*60,
		EscrowExpiration:     now + 48*60*60,
	})
	if err == nil {
		t.Fatal("expected 256th escrow to be rejected by HIVE_MAX_PENDING_TRANSFERS")
	}
	if !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("expected limit-error message, got: %v", err)
	}
}
