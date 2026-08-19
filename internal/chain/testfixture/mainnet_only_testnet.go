//go:build testnet

package testfixture

import (
	"testing"

	"gopherd/internal/protocol/hardfork"
)

// skipIfTestnetSemantics skips fixture-based consensus tests on a testnet
// build.
//
// These fixtures assert MAINNET hardfork semantics: they advance to
// hardfork.MainnetActivationBlock[N] and check behaviour before and after each
// fork. A testnet build zeroes that table because a hived testnet launches with
// every hardfork already applied at block 1 (see hardfork_testnet.go), so
// "before HF20" is not a state a testnet chain can be in, and the scenarios
// have nothing left to assert.
//
// Skipping is the honest outcome — these tests validate mainnet parity, which
// is exercised by the untagged build where they all run.
func skipIfTestnetSemantics(t *testing.T) {
	t.Helper()
	if hardfork.MainnetActivationBlock[hardfork.HF20] == 0 {
		t.Skip("mainnet-semantics fixture: testnet build starts at the latest hardfork")
	}
}
