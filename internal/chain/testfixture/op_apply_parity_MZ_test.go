package testfixture

// Scaffolds the M..Z half of hive_mast/tests/unit/tests/operation_tests.cpp.
//
// The companion file op_apply_parity_AL_test.go (separate agent) handles
// A..L. This file ports the structural-validate cases, skips the
// authorities cases (the fixture deliberately bypasses signature
// verification via SkipTransactionSignatures, so tx_missing_active_auth
// / tx_irrelevant_sig / tx_duplicate_sig can't fire), and stubs out the
// C++ tests that are literally empty in upstream.
//
// Apply-side cases that already have a dedicated test elsewhere
// (TestTransferApply, TestWithdrawVestingApply, TestPowApply*, etc.) are
// not duplicated here.

import (
	"strings"
	"testing"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
)

// ---------- pow ----------

// pow_validate is a literal stub in C++ (`BOOST_TEST_MESSAGE` only). Skipped.
func TestPowValidateCppStub(t *testing.T) {
	t.Skip("MISSING: C++ pow_validate body is just BOOST_TEST_MESSAGE — no assertions to port")
}

// pow_authorities is a literal stub in C++. Skipped.
func TestPowAuthoritiesCppStub(t *testing.T) {
	t.Skip("MISSING: C++ pow_authorities body is just BOOST_TEST_MESSAGE — no assertions to port")
}

// ---------- recurrent_transfer ----------

// recurrent_transfer_validate: structural-validate parity. Mirrors C++
// recurrent_transfer_validate (operation_tests.cpp ~L9408).
func TestRecurrentTransferValidate(t *testing.T) {
	t.Parallel()
	good := op.RecurrentTransfer{
		From:       "alice",
		To:         "bob",
		Memo:       "Memo",
		Recurrence: 24,
		Executions: 10,
		Amount:     asset.Asset{Amount: 100, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("baseline op should validate, got %v", err)
	}

	t.Log("--- Invalid from account")
	bad := good
	bad.From = "alice-"
	if err := bad.Validate(); err == nil {
		t.Fatal("expected invalid from account to fail")
	}

	t.Log("--- Invalid to account")
	bad = good
	bad.To = "bob-"
	if err := bad.Validate(); err == nil {
		t.Fatal("expected invalid to account to fail")
	}

	t.Log("--- Memo too long")
	bad = good
	bad.Memo = strings.Repeat("x", op.MaxMemoSize+1)
	if err := bad.Validate(); err == nil {
		t.Fatal("expected oversize memo to fail")
	}

	t.Log("--- Negative amount")
	bad = good
	bad.Amount.Amount = -100
	if err := bad.Validate(); err == nil {
		t.Fatal("expected negative amount to fail")
	}

	t.Log("--- VESTS amount")
	bad = good
	bad.Amount = asset.Asset{Amount: 100, Symbol: asset.Symbol{Num: asset.VestsSymbol}}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected VESTS amount to fail")
	}

	t.Log("--- Recurrence too low")
	bad = good
	bad.Recurrence = 3
	if err := bad.Validate(); err == nil {
		t.Fatal("expected sub-24h recurrence to fail")
	}

	t.Log("--- Transfer to yourself")
	bad = good
	bad.From = "bob"
	if err := bad.Validate(); err == nil {
		t.Fatal("expected self-transfer to fail")
	}

	t.Log("--- Executions < 2")
	bad = good
	bad.Executions = 1
	if err := bad.Validate(); err == nil {
		t.Fatal("expected executions=1 to fail")
	}
	bad.Executions = 0
	if err := bad.Validate(); err == nil {
		t.Fatal("expected executions=0 to fail")
	}
}

// ---------- signature_stripping ----------
// already covered by TestSignatureStripping.

// ---------- transfer ----------

// transfer_validate: ports operation_tests.cpp::transfer_validate.
func TestTransferValidate(t *testing.T) {
	t.Parallel()
	good := op.Transfer{
		From:   "alice",
		To:     "bob",
		Memo:   "Memo",
		Amount: asset.Asset{Amount: 100, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("baseline transfer should validate, got %v", err)
	}

	t.Log("--- Invalid from account")
	bad := good
	bad.From = "alice-"
	if err := bad.Validate(); err == nil {
		t.Fatal("expected invalid from to fail")
	}

	t.Log("--- Invalid to account")
	bad = good
	bad.To = "bob-"
	if err := bad.Validate(); err == nil {
		t.Fatal("expected invalid to to fail")
	}

	t.Log("--- Memo too long")
	bad = good
	bad.Memo = strings.Repeat("x", op.MaxMemoSize+1)
	if err := bad.Validate(); err == nil {
		t.Fatal("expected oversize memo to fail")
	}

	t.Log("--- Negative amount")
	bad = good
	bad.Amount.Amount = -100
	if err := bad.Validate(); err == nil {
		t.Fatal("expected negative amount to fail")
	}

	t.Log("--- VESTS amount")
	bad = good
	bad.Amount = asset.Asset{Amount: 100, Symbol: asset.Symbol{Num: asset.VestsSymbol}}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected VESTS amount to fail")
	}
}

// transfer_authorities: signature-based. Fixture sets
// SkipTransactionSignatures, so push_transaction throws cannot fire.
func TestTransferAuthoritiesSkip(t *testing.T) {
	t.Skip("MISSING: signature verification disabled in fixture (SkipTransactionSignatures); cannot reproduce tx_missing_active_auth / tx_duplicate_sig / tx_irrelevant_sig")
}

// ---------- transfer_from_savings ----------

// transfer_from_savings_validate: ports operation_tests.cpp::transfer_from_savings_validate.
func TestTransferFromSavingsValidate(t *testing.T) {
	t.Parallel()
	good := op.TransferFromSavings{
		From:      "alice",
		RequestID: 0,
		To:        "alice",
		Amount:    asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("baseline should validate, got %v", err)
	}

	t.Log("--- empty 'from'")
	bad := good
	bad.From = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("expected empty from to fail")
	}

	t.Log("--- empty 'to'")
	bad = good
	bad.To = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("expected empty to to fail")
	}

	t.Log("--- 'to' = bob is OK")
	bad = good
	bad.To = "bob"
	if err := bad.Validate(); err != nil {
		t.Fatalf("to=bob should validate, got %v", err)
	}

	t.Log("--- VESTS amount fails")
	bad = good
	bad.Amount = asset.Asset{Amount: 1_000_000, Symbol: asset.Symbol{Num: asset.VestsSymbol}}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected VESTS amount to fail")
	}

	t.Log("--- HBD amount OK")
	bad = good
	bad.Amount = asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HbdSymbol}}
	if err := bad.Validate(); err != nil {
		t.Fatalf("HBD should validate, got %v", err)
	}

	t.Log("--- HIVE amount OK")
	bad = good
	bad.Amount = asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol}}
	if err := bad.Validate(); err != nil {
		t.Fatalf("HIVE should validate, got %v", err)
	}
}

func TestTransferFromSavingsAuthoritiesSkip(t *testing.T) {
	t.Skip("MISSING: signature verification disabled in fixture (SkipTransactionSignatures)")
}

// ---------- transfer_to_savings ----------

// transfer_to_savings_validate: ports operation_tests.cpp::transfer_to_savings_validate.
func TestTransferToSavingsValidate(t *testing.T) {
	t.Parallel()
	good := op.TransferToSavings{
		From:   "alice",
		To:     "alice",
		Amount: asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("baseline should validate, got %v", err)
	}

	t.Log("--- empty 'from'")
	bad := good
	bad.From = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("expected empty from to fail")
	}

	t.Log("--- empty 'to'")
	bad = good
	bad.To = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("expected empty to to fail")
	}

	t.Log("--- 'to' = bob is OK")
	bad = good
	bad.To = "bob"
	if err := bad.Validate(); err != nil {
		t.Fatalf("to=bob should validate, got %v", err)
	}

	t.Log("--- VESTS amount fails")
	bad = good
	bad.Amount = asset.Asset{Amount: 1_000_000, Symbol: asset.Symbol{Num: asset.VestsSymbol}}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected VESTS amount to fail")
	}

	t.Log("--- HBD amount OK")
	bad = good
	bad.Amount = asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HbdSymbol}}
	if err := bad.Validate(); err != nil {
		t.Fatalf("HBD should validate, got %v", err)
	}

	t.Log("--- HIVE amount OK")
	bad = good
	bad.Amount = asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol}}
	if err := bad.Validate(); err != nil {
		t.Fatalf("HIVE should validate, got %v", err)
	}
}

func TestTransferToSavingsAuthoritiesSkip(t *testing.T) {
	t.Skip("MISSING: signature verification disabled in fixture (SkipTransactionSignatures)")
}

// ---------- transfer_to_vesting ----------

// transfer_to_vesting_validate is essentially empty in C++ (just
// validate_database()). Port a minimal structural check on the Go
// validator instead.
func TestTransferToVestingValidate(t *testing.T) {
	t.Parallel()
	good := op.TransferToVesting{
		From:   "alice",
		To:     "bob",
		Amount: asset.Asset{Amount: 2500, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("baseline should validate, got %v", err)
	}

	bad := good
	bad.From = "alice-"
	if err := bad.Validate(); err == nil {
		t.Fatal("expected invalid from to fail")
	}

	bad = good
	bad.Amount = asset.Asset{Amount: 2500, Symbol: asset.Symbol{Num: asset.HbdSymbol}}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected HBD amount to fail (must be HIVE)")
	}

	bad = good
	bad.Amount.Amount = -1
	if err := bad.Validate(); err == nil {
		t.Fatal("expected negative amount to fail")
	}
}

func TestTransferToVestingAuthoritiesSkip(t *testing.T) {
	t.Skip("MISSING: signature verification disabled in fixture (SkipTransactionSignatures)")
}

// ---------- vote ----------

// vote_validate: C++ body is just validate_database(). Port a minimal
// structural check on the Go validator.
func TestVoteValidate(t *testing.T) {
	t.Parallel()
	good := op.Vote{
		Voter:    "alice",
		Author:   "bob",
		Permlink: "post",
		Weight:   10000,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("baseline should validate, got %v", err)
	}

	bad := good
	bad.Voter = "alice-"
	if err := bad.Validate(); err == nil {
		t.Fatal("expected invalid voter to fail")
	}

	bad = good
	bad.Permlink = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("expected empty permlink to fail")
	}

	bad = good
	bad.Permlink = strings.Repeat("p", op.MaxPermlinkLength+1)
	if err := bad.Validate(); err == nil {
		t.Fatal("expected oversize permlink to fail")
	}

	bad = good
	bad.Weight = op.VoteWeightMax + 1
	if err := bad.Validate(); err == nil {
		t.Fatal("expected weight > 10000 to fail")
	}

	bad = good
	bad.Weight = op.VoteWeightMin - 1
	if err := bad.Validate(); err == nil {
		t.Fatal("expected weight < -10000 to fail")
	}
}

// vote_authorities: C++ body is just validate_database(); nothing to assert.
func TestVoteAuthoritiesCppStub(t *testing.T) {
	t.Skip("MISSING: C++ vote_authorities body is just validate_database() — no assertions to port")
}

// ---------- withdraw_vesting ----------

// withdraw_vesting_validate: C++ body is just validate_database().
// Port a minimal structural check.
func TestWithdrawVestingValidate(t *testing.T) {
	t.Parallel()
	good := op.WithdrawVesting{
		Account:       "alice",
		VestingShares: asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.VestsSymbol}},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("baseline should validate, got %v", err)
	}

	bad := good
	bad.Account = "alice-"
	if err := bad.Validate(); err == nil {
		t.Fatal("expected invalid account to fail")
	}

	bad = good
	bad.VestingShares = asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol}}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected HIVE-symbol vesting_shares to fail")
	}
}

func TestWithdrawVestingAuthoritiesSkip(t *testing.T) {
	t.Skip("MISSING: signature verification disabled in fixture (SkipTransactionSignatures)")
}

// ---------- witness_set_properties ----------

// witness_set_properties_validate: ports the structural-only subset of
// the C++ test. The deeper checks (asset symbol packed inside props
// blobs, max-block-size lower bound, hbd_interest_rate range, exchange-
// rate asymmetry, UTF-8 url validation, account_subsidy_budget range,
// account_subsidy_decay range) are NOT implemented in the Go validator
// yet — those branches are gated behind the C++ packed-blob inspector.
func TestWitnessSetPropertiesValidate(t *testing.T) {
	t.Parallel()
	t.Log("--- failure when 'key' prop is absent")
	bad := op.WitnessSetProperties{Owner: "alice"}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected missing-key to fail")
	}

	t.Log("--- success when 'key' prop is present")
	good := op.WitnessSetProperties{
		Owner: "alice",
		Props: []op.WitnessProperty{{Key: "key", Value: make([]byte, 33)}},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("with 'key' should validate, got %v", err)
	}

	t.Log("--- invalid owner account name")
	bad = op.WitnessSetProperties{
		Owner: "alice-",
		Props: []op.WitnessProperty{{Key: "key", Value: make([]byte, 33)}},
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected bad owner to fail")
	}
}

func TestWitnessSetPropertiesPropBlobInspectionMissing(t *testing.T) {
	t.Skip("MISSING: Go validator does not inspect packed prop blobs (account_creation_fee symbol, maximum_block_size lower bound, hbd_interest_rate range, exchange_rate asymmetry, url UTF-8, account_subsidy_budget range, account_subsidy_decay range)")
}

func TestWitnessSetPropertiesAuthoritiesSkip(t *testing.T) {
	t.Skip("MISSING: signature verification disabled in fixture (SkipTransactionSignatures)")
}

// ---------- witness_update ----------

// witness_update_validate: C++ body is just validate_database().
// Port a minimal structural check on the Go validator.
func TestWitnessUpdateValidate(t *testing.T) {
	t.Parallel()
	good := op.WitnessUpdate{
		Owner: "alice",
		URL:   "foo.bar",
		Props: op.LegacyChainProperties{
			AccountCreationFee: LegacyHIVE(1_000),
			MaximumBlockSize:   65536,
			HBDInterestRate:    1000,
		},
		Fee: asset.Asset{Amount: 0, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("baseline should validate, got %v", err)
	}

	bad := good
	bad.Owner = "alice-"
	if err := bad.Validate(); err == nil {
		t.Fatal("expected bad owner to fail")
	}

	bad = good
	bad.URL = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("expected empty URL to fail")
	}

	bad = good
	bad.Fee.Amount = -1
	if err := bad.Validate(); err == nil {
		t.Fatal("expected negative fee to fail")
	}
}

func TestWitnessUpdateAuthoritiesSkip(t *testing.T) {
	t.Skip("MISSING: signature verification disabled in fixture (SkipTransactionSignatures)")
}
