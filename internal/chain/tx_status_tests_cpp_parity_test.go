package chain

import "testing"

// Skipped ports from hive/tests/unit/tests/tx_status_tests.cpp.
//
// The C++ suite verifies that database::get_tx_status() and the family
// of is_validating_block / is_producing_block / is_replaying_block /
// is_processing_block / is_validating_tx / is_validating_one_tx /
// is_reapplying_tx / is_reapplying_one_tx / is_in_control flags report
// the correct combination through every pre/post-apply
// {block,transaction} signal slot, for:
//
//   - regular pushed transactions, including those evicted to the
//     pending list when the block fills,
//   - transactions popped on undo,
//   - transactions reapplied during a fork switch,
//   - the failure-during-fork-switch path.
//
// gopherd has no equivalent state-machine. The chain pipeline
// (internal/chain/database.go, evaluator package, hooks) is driven
// entirely by direct calls; there are no pre/post-apply signal slots,
// no transaction_status enum exposed via the session, and no
// pending-transaction reapplication loop visible to test fixtures.
// The replay-vs-live distinction is captured by the SkipSig /
// SkipTxDupeCheck flags on state.Session rather than a per-step status.
//
// If a notification framework + tx_status enum ever lands, port these
// four cases from upstream tx_status_tests.cpp — the expectation matrix
// in struct expectation::verify() is the reference for which flags
// should be set at each slot.

// TestSkippedTxStatusRegularTransactions — regular_transactions:
// pre/post-TX flag checks across a normal comment_operation push, a
// failed push (parent missing), and the 3-large-comment block-fill
// scenario that defers two txs to the pending list.
func TestSkippedTxStatusRegularTransactions(t *testing.T) {
	t.Skip("no Go tx_status enum / pre-post-apply signal framework; pending-tx loop n/a")
}

// TestSkippedTxStatusPoppedTransactions — popped_transactions: pushes
// transactions, generates blocks, then triggers undo (pop_block) and
// asserts the popped transactions resurface as pending with the
// correct TX_STATUS_PENDING flags.
func TestSkippedTxStatusPoppedTransactions(t *testing.T) {
	t.Skip("no Go pop_block + pending-tx resurrection path; undo replays bytes only")
}

// TestSkippedTxStatusTransactionsInForks — transactions_in_forks: fork
// switch reapplies committed transactions on the new branch and pops
// those exclusive to the old branch; verifies TX_STATUS_BLOCK on
// reapply and TX_STATUS_PENDING on the popped set.
func TestSkippedTxStatusTransactionsInForks(t *testing.T) {
	t.Skip("no Go fork-switch reapply-tx path with TX_STATUS_BLOCK tracking")
}

// TestSkippedTxStatusFailureDuringForkSwitch — failure_during_fork_switch:
// injects a soft and a hard failure into the pre-apply-block hook
// mid-fork-switch; asserts the original chain is restored intact and
// the failure surfaces with the correct status flags.
func TestSkippedTxStatusFailureDuringForkSwitch(t *testing.T) {
	t.Skip("no Go pre-apply-block failure-injection hook; covered by p2p reject-on-error path")
}
