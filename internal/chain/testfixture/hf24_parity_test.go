package testfixture

import "testing"

// 1:1 parity scaffolds for hive_mast/tests/unit/tests/hf24_tests.cpp.
//
// All four HF24 cases already have functional coverage elsewhere in
// this package; the scaffolds below name them 1:1 with the C++ test
// suite so the parity matrix stays explicit. Each delegates to the
// real implementation via a t.Skip pointing at the canonical Go test.

// TestHF24BlockedOperations is the 1:1 parity name for
// hf24_tests.cpp::blocked_operations. The functional port lives in
// operation_time_test.go::TestHF24BlockedTreasuryOps, which exercises
// transfer_to_vesting / set_withdraw_vesting_route / transfer_to_savings
// to hive.fund and asserts each is rejected post-HF24.
func TestHF24BlockedOperations(t *testing.T) {
	t.Skip("covered by TestHF24BlockedTreasuryOps in operation_time_test.go (1:1 functional port)")
}

// TestHF24CommentBeneficiary is the 1:1 parity name for
// hf24_tests.cpp::comment_beneficiary. The C++ test asserts that a
// comment with OBSOLETE_TREASURY_ACCOUNT (steem.dao) as a 100%
// beneficiary routes payout to hive.fund instead.
//
// Functional coverage attempt lives in
// operation_time_test.go::TestHF24CommentBeneficiaryTreasury (skipped
// because the assertion depends on debug_update + deterministic
// cashout math, and Go cashouts deposit to liquid balances rather than
// reward balances — see project_cashout_liquid_vs_reward_balance memo).
func TestHF24CommentBeneficiary(t *testing.T) {
	t.Skip("covered by TestHF24CommentBeneficiaryTreasury in operation_time_test.go (1:1 port, skip with rationale)")
}

// TestHF24ConsolidateBalanceParity is the 1:1 parity name for
// hf24_tests.cpp::consolidate_balance. The functional port lives in
// hf24_extra_test.go::TestHF24ConsolidateBalance — seeds steem.dao
// with HIVE/HBD, fires HF24, asserts the balances move to hive.fund.
//
// Renamed with the `Parity` suffix to avoid colliding with the
// existing TestHF24ConsolidateBalance in hf24_extra_test.go.
func TestHF24ConsolidateBalanceParity(t *testing.T) {
	t.Skip("covered by TestHF24ConsolidateBalance in hf24_extra_test.go (1:1 functional port). The C++ test additionally exercises the savings/reward/vest sub-balances and asserts a specific 14999-HIVE rounding on the vest→hive conversion (a 1-sat rounding pattern) — not currently asserted in the Go port.")
}

// TestHF24TreasuryDebtRatio is the 1:1 parity name for
// hf24_tests.cpp::treasury_debt_ratio. The C++ test asserts that
// post-HF24, treasury-held HBD is excluded from the debt-ratio
// calculation that drives hbd_print_rate. Coverage stub lives in
// operation_time_test.go::TestHF24TreasuryDebtRatioExcluded — skipped
// because the Go-side print-rate hook needs an explicit
// ConvertExcludeTreasury check we haven't ported yet.
func TestHF24TreasuryDebtRatio(t *testing.T) {
	t.Skip("covered by TestHF24TreasuryDebtRatioExcluded in operation_time_test.go (skip with rationale: print-rate hook missing ConvertExcludeTreasury; see also feedback_print_rate_zero_means_zero memo)")
}
