package testfixture

import (
	"strings"
	"testing"

	"hived-go/internal/protocol/op"
)

// TestCustomJSONRateLimit ports operation_tests.cpp::custom_json_rate_limit
// for the ApplyBlock path. HIVE_CUSTOM_OP_BLOCK_LIMIT (=5) is a
// mempool-only rule in hived: custom_op_visitor at database.cpp:578-616
// fires from _push_transaction at :632, not from _apply_block.
// Block-apply trusts the producing witness, so blocks legitimately
// carry >5 custom_json per account on mainnet (and replay must accept
// them). The push_transaction-path enforcement is a separate feature
// (ghive does not yet have a push_transaction entrypoint) and will get
// its own test alongside that wiring.
func TestCustomJSONRateLimit(t *testing.T) {
	f := New(t)
	f.Actors("alice", "sam")
	f.GenerateBlock()

	t.Log("--- 5 custom_json ops in one block-tx for alice: succeeds")
	for i := 0; i < 5; i++ {
		f.PushOp(op.CustomJSON{
			RequiredPostingAuths: []string{"alice"},
			ID:                   "test",
			JSON:                 `"hi"`,
		})
	}
	if err := f.tryGenerateBlock(); err != nil {
		t.Fatalf("expected 5 ops for alice to succeed, got: %v", err)
	}

	t.Log("--- 6 in one block-tx for sam: ALSO succeeds (cap is push_transaction-only)")
	for i := 0; i < 6; i++ {
		f.PushOp(op.CustomJSON{
			RequiredPostingAuths: []string{"sam"},
			ID:                   "test",
			JSON:                 `"manyops"`,
		})
	}
	if err := f.tryGenerateBlock(); err != nil {
		t.Fatalf("expected 6 ops for sam to succeed under ApplyBlock (cap is mempool-only), got: %v", err)
	}
	_ = strings.Contains // keep import; will be needed by the push_transaction port
}
