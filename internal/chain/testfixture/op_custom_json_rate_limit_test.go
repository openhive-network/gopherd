package testfixture

import (
	"strings"
	"testing"

	"hived-go/internal/protocol/op"
)

// TestCustomJSONRateLimit ports operation_tests.cpp::custom_json_rate_limit:
// the chain caps each account at HIVE_CUSTOM_OP_BLOCK_LIMIT (=5) custom*
// operations per block. The 6th custom_json for the same account in the
// same block must fail.
//
// C++ test pushes 3 scenarios:
//   1. 5 custom_jsons in 5 separate txs (same block context) — success.
//   2. 5 custom_jsons in 1 tx — success.
//   3. 6 custom_jsons in 1 tx — failure on "insert_info.first->second <=
//      HIVE_CUSTOM_OP_BLOCK_LIMIT".
//
// The Go fixture packs all pending ops into ONE transaction per block, so
// scenarios (1) and (2) collapse to the same observable behavior: 5 ops in
// one block-tx for alice succeeds; 6 ops in one block-tx for sam fails.
// We additionally verify the cap RESETS each block by pushing 5 more for
// alice in a fresh block immediately after the first 5.
func TestCustomJSONRateLimit(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob", "sam")
	f.GenerateBlock()

	t.Log("--- 5 custom_json ops in one tx for alice: succeeds")
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

	t.Log("--- another 5 in the NEXT block for alice: succeeds (counter resets per block)")
	for i := 0; i < 5; i++ {
		f.PushOp(op.CustomJSON{
			RequiredPostingAuths: []string{"alice"},
			ID:                   "test",
			JSON:                 `"again"`,
		})
	}
	if err := f.tryGenerateBlock(); err != nil {
		t.Fatalf("expected 5 ops for alice in fresh block to succeed, got: %v", err)
	}

	t.Log("--- 5 in one tx for bob: succeeds")
	for i := 0; i < 5; i++ {
		f.PushOp(op.CustomJSON{
			RequiredPostingAuths: []string{"bob"},
			ID:                   "test",
			JSON:                 `"foobar"`,
		})
	}
	if err := f.tryGenerateBlock(); err != nil {
		t.Fatalf("expected 5 ops for bob to succeed, got: %v", err)
	}

	t.Log("--- 6 in one tx for sam: fails with HIVE_CUSTOM_OP_BLOCK_LIMIT")
	for i := 0; i < 6; i++ {
		f.PushOp(op.CustomJSON{
			RequiredPostingAuths: []string{"sam"},
			ID:                   "test",
			JSON:                 `"toomany"`,
		})
	}
	err := f.tryGenerateBlock()
	if err == nil {
		t.Fatalf("expected 6 ops for sam to fail")
	}
	// The error chain should mention the cap. We don't pin the exact
	// wording — only that it references "custom json" or the cap value.
	if !strings.Contains(err.Error(), "custom json") && !strings.Contains(err.Error(), "custom_json") {
		t.Fatalf("expected error to reference custom_json rate limit, got: %v", err)
	}
}
