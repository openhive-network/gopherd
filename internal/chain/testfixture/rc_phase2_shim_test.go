package testfixture

// Shim bindings for the RC Phase 2 tests in op_rc_phase2_test.go.
// The test file calls these helpers as lowercase package-local
// identifiers, but the implementations live in package `chain` and are
// exported with capitalized names (Process…ForTest). Re-export them as
// lowercase symbols local to the test package so the file links.

import (
	"gopherd/internal/chain"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

func processRCExpiredDelegationsForTest(s *state.Session, now uint32) {
	chain.ProcessRCExpiredDelegationsForTest(s, now)
}

func rotateRCUsageBucketsForTest(s *state.Session, now uint32) {
	chain.RotateRCUsageBucketsForTest(s, now)
}

func finalizeRCStatsForTest(s *state.Session, blockNum uint32) {
	chain.FinalizeRCStatsForTest(s, blockNum)
}

func handleVestingWithdrawalRCImpactForTest(s *state.Session, a *objects.Account, now uint32) {
	chain.HandleVestingWithdrawalRCImpactForTest(s, a, now)
}
