package testfixture

// 1:1 parity scaffold against hive_mast/tests/unit/tests/block_tests.cpp.
//
// The C++ file contains the following BOOST_(AUTO|FIXTURE)_TEST_CASEs:
//   - generate_empty_blocks
//   - undo_block
//   - fork_blocks
//   - switch_forks_undo_create
//   - duplicate_transactions
//   - tapos
//   - optional_tapos
//   - double_sign_check
//   - pop_block_twice
//   - rsf_missed_blocks
//   - skip_block
//   - hardfork_test
//   - generate_block_size
//
// Most are already ported in:
//   - block_cpp_parity_test.go : duplicate_transactions, generate_block_size,
//     tapos (slot side), skip_block (DGPO side), init_hive_hbd_supply.
//   - block_extra_test.go : tapos (ring side), optional_tapos, hardfork_test,
//     skip_block (gap side), set_lower_lib_then_current; plus Skip stubs for
//     undo_block, pop_block_twice, fork_blocks, switch_forks_undo_create,
//     switch_forks_using_fast_confirm, fast_confirm_plus_out_of_order_blocks,
//     rsf_missed_blocks, double_sign_check, block_flow_control_*.
//
// The remaining case (generate_empty_blocks) is added below. The Skip-with-
// MISSING entries are grouped here too so a single file gives a quick view of
// the parity status without scanning block_extra_test.go.

import (
	"testing"
)

// TestBlockGenerateEmptyBlocks ports block_tests.cpp::generate_empty_blocks.
//
// The C++ test grinds blocks until LastIrreversibleBlockNum reaches 200,
// records that "cutoff" block, closes the database, re-opens it and verifies
// HeadBlockNumber matches the cutoff. It then generates another 200 blocks
// and asserts HeadBlockNumber bumps by exactly 200.
//
// Go has no Chain.Close / Chain.Open round-trip in the test fixture (the
// state.Database is reopenable, but plugging it back into a Fixture is not
// wired up). We exercise the in-memory part of the invariant — grinding
// enough blocks to drive LIB ≥ 200, then another 200, asserting head_block_num
// advances in lockstep and LIB monotonically rises — which is the substance of
// the C++ test minus the close/reopen leg.
func TestBlockGenerateEmptyBlocks(t *testing.T) {
	f := New(t)

	// Phase 1: grind until LastIrreversibleBlockNum hits 200. With 1 init
	// witness this takes ~200 blocks (LIB == head_block_num pre-multi-witness
	// in the test fixture). Hardcap to prevent runaway in CI.
	var cutoffHead uint32
	for i := 0; i < 1000; i++ {
		f.GenerateBlock()
		dgpo := f.DGPO()
		if dgpo.LastIrreversibleBlockNum >= 200 {
			cutoffHead = dgpo.HeadBlockNumber
			break
		}
	}
	if cutoffHead == 0 {
		t.Fatalf("LIB never reached 200 after 1000 blocks (LIB=%d head=%d)",
			f.DGPO().LastIrreversibleBlockNum, f.DGPO().HeadBlockNumber)
	}

	// Phase 2: grind another 200 blocks. head_block_num should advance by
	// exactly 200, and LIB must never regress.
	prevLIB := f.DGPO().LastIrreversibleBlockNum
	for i := 0; i < 200; i++ {
		f.GenerateBlock()
		curLIB := f.DGPO().LastIrreversibleBlockNum
		if curLIB < prevLIB {
			t.Fatalf("LIB regressed at block %d: %d → %d",
				f.DGPO().HeadBlockNumber, prevLIB, curLIB)
		}
		prevLIB = curLIB
	}

	if got, want := f.DGPO().HeadBlockNumber, cutoffHead+200; got != want {
		t.Errorf("HeadBlockNumber after +200: got %d, want %d", got, want)
	}

	// The close/reopen leg of the C++ test is not portable to the Go fixture
	// in its current shape — a fixture-level Close + reattach would be needed.
	// See block_extra_test.go::TestBlockSafeClosingDatabase for the closest
	// proxy (open + genesis.Apply + Close + reopen + Close on a bare
	// state.Database).
}

// The cases below are 1:1 placeholders so a scan of this file reflects the
// full 13-case roster from block_tests.cpp. The actual asserting code lives
// in block_cpp_parity_test.go / block_extra_test.go — these scaffolds delegate
// via t.Skip pointing at the canonical implementation.

func TestBlockGenerateEmptyBlocksAlreadyCovered(t *testing.T) {
	t.Skip("covered above: TestBlockGenerateEmptyBlocks")
}

func TestBlockUndoBlockParity(t *testing.T) {
	t.Skip("MISSING: Go has no Chain.PopBlock — chain.Database.ApplyBlock " +
		"commits through state.Session.Commit. Block-boundary rollback would " +
		"require a per-block undo stack at the chain layer. See " +
		"block_extra_test.go::TestBlockUndoBlock.")
}

func TestBlockForkBlocksParity(t *testing.T) {
	t.Skip("MISSING: Go has no fork_database / fork-switching: chain.Database " +
		"applies one canonical chain forward via ApplyBlock and does not " +
		"maintain a fork DB. See block_extra_test.go::TestBlockForkBlocks.")
}

func TestBlockSwitchForksUndoCreateParity(t *testing.T) {
	t.Skip("MISSING: depends on fork_database/fork-switching infrastructure; " +
		"see TestBlockForkBlocksParity.")
}

func TestBlockDuplicateTransactionsParity(t *testing.T) {
	t.Skip("covered: TestDuplicateTransactions in block_cpp_parity_test.go.")
}

func TestBlockTaposParity(t *testing.T) {
	t.Skip("covered: TestTaposSlotPopulated (slot side) in " +
		"block_cpp_parity_test.go + TestBlockTaposRingPopulatedAcrossBlocks " +
		"in block_extra_test.go.")
}

func TestBlockOptionalTaposParity(t *testing.T) {
	t.Skip("covered: TestBlockOptionalTaposZeroRefBlockNum in block_extra_test.go.")
}

func TestBlockDoubleSignCheckParity(t *testing.T) {
	t.Skip("MISSING: Go's VerifyAuthority does not reject duplicate/" +
		"irrelevant signatures yet — see op_signature_stripping_test.go and " +
		"verify_authority.go. Porting requires sign_state::unused_signatures() " +
		"equivalent. Skip mirrored in block_extra_test.go::TestBlockDoubleSignCheck.")
}

func TestBlockPopBlockTwiceParity(t *testing.T) {
	t.Skip("MISSING: Go has no Chain.PopBlock; see TestBlockUndoBlockParity.")
}

func TestBlockRSFMissedBlocksParity(t *testing.T) {
	t.Skip("MISSING: hived-go's DGPO does not yet carry recent_slots_filled or " +
		"a witness_participation_rate getter (see " +
		"internal/state/objects/dgpo.go). Skip mirrored in " +
		"block_extra_test.go::TestBlockRSFMissedBlocks.")
}

func TestBlockSkipBlockParity(t *testing.T) {
	t.Skip("covered: TestSkipBlockMatchesPriorState (DGPO side) in " +
		"block_cpp_parity_test.go + TestBlockSkipBlockGap (gap side) in " +
		"block_extra_test.go.")
}

func TestBlockHardforkTestParity(t *testing.T) {
	t.Skip("covered: TestBlockHardforkApplied in block_extra_test.go.")
}

func TestBlockGenerateBlockSizeParity(t *testing.T) {
	t.Skip("covered: TestGenerateBlockSize in block_cpp_parity_test.go.")
}
