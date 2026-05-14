package testfixture

// Scaffolds the M..Z half of hive_mast/tests/unit/tests/operation_tests.cpp.
//
// The companion file op_apply_parity_AL_test.go (separate agent) handles
// A..L. This file ports the structural-validate cases, the *_authorities
// cases (via chain.VerifyAuthority + chain.RequiredAuthorities — same
// surface that backs hived's push_transaction sig-failure exceptions),
// and stubs out the C++ tests that are literally empty in upstream.
//
// Apply-side cases that already have a dedicated test elsewhere
// (TestTransferApply, TestWithdrawVestingApply, TestPowApply*, etc.) are
// not duplicated here.

import (
	"errors"
	"strings"
	"testing"

	"hived-go/internal/chain"
	"hived-go/internal/crypto"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/network"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
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

// TestTransferAuthorities ports operation_tests.cpp::transfer_authorities.
//
// The C++ test pushes a transfer op signed under various wrong key
// combinations and asserts each rejection class:
//
//   - no signatures             → tx_missing_active_auth
//   - signed by alice_post_key  → tx_missing_active_auth
//   - signed by alice + alice   → tx_duplicate_sig
//   - signed by alice + bob     → tx_irrelevant_sig
//   - signed by alice (active)  → succeeds
//
// We exercise this through chain.VerifyAuthority directly (same pattern
// as op_signature_stripping_test.go). Going through ApplyBlock would
// require multi-attempt rollback of the same block num, which the
// fixture's tryGenerateBlock doesn't model; chain.VerifyAuthority IS
// the function that raises tx_missing_active_auth / tx_duplicate_sig /
// tx_irrelevant_sig in hived's push_transaction path.
func TestTransferAuthorities(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 10_000)
	f.GenerateBlock()

	aliceActive := f.ActorKey("alice")
	alicePosting := f.ActorPostingKey("alice")
	bobActive := f.ActorKey("bob")

	body, unsigned := buildTxBody(f, op.Transfer{
		From:   "alice",
		To:     "bob",
		Amount: HIVE(2_500),
	})

	t.Log("--- Test failure when no signatures (tx_missing_active_auth)")
	requireMissingActive(t, f, body, unsigned)

	t.Log("--- Test failure when signed by a signature not in the account's authority (tx_missing_active_auth)")
	stxWrong := signTx(t, f, body, unsigned, alicePosting)
	requireVerifyErr(t, f, &stxWrong, unsigned, "active", false)

	t.Log("--- Test failure when duplicate signatures (tx_duplicate_sig)")
	stxDup := signTx(t, f, body, unsigned, aliceActive, aliceActive)
	requireDuplicateSig(t, f, &stxDup, unsigned)

	t.Log("--- Test failure when signed by an additional signature not in the creator's authority (tx_irrelevant_sig)")
	stxIrrel := signTx(t, f, body, unsigned, aliceActive, bobActive)
	requireIrrelevantSig(t, f, &stxIrrel, unsigned)

	t.Log("--- Test success with alice's active signature")
	stxOK := signTx(t, f, body, unsigned, aliceActive)
	if err := chain.VerifyAuthority(state.NewSession(f.State), &stxOK, unsigned, network.OldChainID, false); err != nil {
		t.Fatalf("active-signed transfer should verify: %v", err)
	}
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

// TestTransferFromSavingsAuthorities ports
// operation_tests.cpp::transfer_from_savings_authorities. The C++ test
// is structural: it checks that get_required_owner_authorities and
// get_required_posting_authorities return empty sets and
// get_required_active_authorities returns the {from} singleton.
//
// In Go the equivalent is chain.RequiredAuthorities, which buckets each
// op by authority level.
func TestTransferFromSavingsAuthorities(t *testing.T) {
	t.Parallel()
	stx := singleOpTx(op.TransferFromSavings{
		From:      "alice",
		To:        "alice",
		Amount:    HIVE(1_000),
		RequestID: 0,
	})
	required := chain.RequiredAuthorities(&stx)
	requireSetEqual(t, "owner", required[chain.AuthorityOwner], nil)
	requireSetEqual(t, "posting", required[chain.AuthorityPosting], nil)
	requireSetEqual(t, "active", required[chain.AuthorityActive], []string{"alice"})

	stx2 := singleOpTx(op.TransferFromSavings{
		From:      "bob",
		To:        "alice",
		Amount:    HIVE(1_000),
		RequestID: 1,
	})
	required2 := chain.RequiredAuthorities(&stx2)
	requireSetEqual(t, "active(from=bob)", required2[chain.AuthorityActive], []string{"bob"})
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

// TestTransferToSavingsAuthorities ports
// operation_tests.cpp::transfer_to_savings_authorities. Structural:
// asserts get_required_active_authorities returns {from} and the
// owner/posting sets are empty.
func TestTransferToSavingsAuthorities(t *testing.T) {
	t.Parallel()
	stx := singleOpTx(op.TransferToSavings{
		From:   "alice",
		To:     "alice",
		Amount: HIVE(1_000),
	})
	required := chain.RequiredAuthorities(&stx)
	requireSetEqual(t, "owner", required[chain.AuthorityOwner], nil)
	requireSetEqual(t, "posting", required[chain.AuthorityPosting], nil)
	requireSetEqual(t, "active", required[chain.AuthorityActive], []string{"alice"})

	stx2 := singleOpTx(op.TransferToSavings{
		From:   "bob",
		To:     "alice",
		Amount: HIVE(1_000),
	})
	required2 := chain.RequiredAuthorities(&stx2)
	requireSetEqual(t, "active(from=bob)", required2[chain.AuthorityActive], []string{"bob"})
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

// TestTransferToVestingAuthorities ports
// operation_tests.cpp::transfer_to_vesting_authorities. Exercises the
// same five sig-failure / sig-success paths as transfer_authorities.
func TestTransferToVestingAuthorities(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 10_000)
	f.GenerateBlock()

	aliceActive := f.ActorKey("alice")
	alicePosting := f.ActorPostingKey("alice")
	bobActive := f.ActorKey("bob")

	body, unsigned := buildTxBody(f, op.TransferToVesting{
		From:   "alice",
		To:     "bob",
		Amount: HIVE(2_500),
	})

	t.Log("--- Test failure when no signatures (tx_missing_active_auth)")
	requireMissingActive(t, f, body, unsigned)

	t.Log("--- Test failure when signed by posting key (tx_missing_active_auth)")
	stxWrong := signTx(t, f, body, unsigned, alicePosting)
	requireVerifyErr(t, f, &stxWrong, unsigned, "active", false)

	t.Log("--- Test failure when duplicate signatures (tx_duplicate_sig)")
	stxDup := signTx(t, f, body, unsigned, aliceActive, aliceActive)
	requireDuplicateSig(t, f, &stxDup, unsigned)

	t.Log("--- Test failure with additional bob signature (tx_irrelevant_sig)")
	stxIrrel := signTx(t, f, body, unsigned, aliceActive, bobActive)
	requireIrrelevantSig(t, f, &stxIrrel, unsigned)

	t.Log("--- Test success with alice active signature")
	stxOK := signTx(t, f, body, unsigned, aliceActive)
	if err := chain.VerifyAuthority(state.NewSession(f.State), &stxOK, unsigned, network.OldChainID, false); err != nil {
		t.Fatalf("active-signed transfer_to_vesting should verify: %v", err)
	}
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

// TestWithdrawVestingAuthorities ports
// operation_tests.cpp::withdraw_vesting_authorities. Exercises the
// no-sig / wrong-key / duplicate / irrelevant / success paths against
// withdraw_vesting (active authority).
func TestWithdrawVestingAuthorities(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 10_000)
	f.FundVesting("alice", 10_000)
	f.GenerateBlock()

	aliceActive := f.ActorKey("alice")
	alicePosting := f.ActorPostingKey("alice")
	bobActive := f.ActorKey("bob")

	body, unsigned := buildTxBody(f, op.WithdrawVesting{
		Account:       "alice",
		VestingShares: VESTS(1_000),
	})

	t.Log("--- Test failure when no signature (tx_missing_active_auth)")
	requireMissingActive(t, f, body, unsigned)

	t.Log("--- Test success with active signature")
	stxOK := signTx(t, f, body, unsigned, aliceActive)
	if err := chain.VerifyAuthority(state.NewSession(f.State), &stxOK, unsigned, network.OldChainID, false); err != nil {
		t.Fatalf("active-signed withdraw_vesting should verify: %v", err)
	}

	t.Log("--- Test failure with duplicate signature (tx_duplicate_sig)")
	stxDup := signTx(t, f, body, unsigned, aliceActive, aliceActive)
	requireDuplicateSig(t, f, &stxDup, unsigned)

	t.Log("--- Test failure with additional incorrect signature (tx_irrelevant_sig)")
	stxIrrel := signTx(t, f, body, unsigned, aliceActive, bobActive)
	requireIrrelevantSig(t, f, &stxIrrel, unsigned)

	t.Log("--- Test failure with incorrect signature (tx_missing_active_auth)")
	stxPost := signTx(t, f, body, unsigned, alicePosting)
	requireVerifyErr(t, f, &stxPost, unsigned, "active", false)
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

// TestWitnessSetPropertiesAuthorities ports the structural half of
// operation_tests.cpp::witness_set_properties_authorities: the op
// requires NO account-name authority (owner, active, posting are all
// empty sets). It requires a direct key-signature against the `key`
// prop blob, falling back to HIVE_NULL_ACCOUNT (unsatisfiable) when
// the prop is absent.
//
// In Go, RequiredAuthorities classifies witness_set_properties under
// no level (PayerOf returns ""), so all three buckets are empty —
// matching the C++ structural assertion.
//
// The C++ test's get_required_authorities (key-authority side) path
// is not exercised in Go because chain.RequiredAuthorities currently
// only buckets by AccountName-level authority; the per-op
// authority<key> override used by witness_set_properties isn't yet
// modelled in Go's verifier. See TestWitnessSetPropertiesAuthoritiesKeyAuthMissing.
func TestWitnessSetPropertiesAuthorities(t *testing.T) {
	t.Parallel()
	stx := singleOpTx(op.WitnessSetProperties{
		Owner: "alice",
		Props: []op.WitnessProperty{{Key: "key", Value: make([]byte, 33)}},
	})
	required := chain.RequiredAuthorities(&stx)
	requireSetEqual(t, "owner", required[chain.AuthorityOwner], nil)
	requireSetEqual(t, "active", required[chain.AuthorityActive], nil)
	requireSetEqual(t, "posting", required[chain.AuthorityPosting], nil)

	// And the same with the "key" prop absent — still no account-level
	// auth required.
	stx2 := singleOpTx(op.WitnessSetProperties{Owner: "alice"})
	required2 := chain.RequiredAuthorities(&stx2)
	requireSetEqual(t, "owner(no-key)", required2[chain.AuthorityOwner], nil)
	requireSetEqual(t, "active(no-key)", required2[chain.AuthorityActive], nil)
	requireSetEqual(t, "posting(no-key)", required2[chain.AuthorityPosting], nil)
}

// TestWitnessSetPropertiesAuthoritiesKeyAuthMissing pins the gap: the
// C++ test also asserts that get_required_authorities returns the
// per-op key-authority (authority(1, signing_key, 1) when 'key' is
// set; authority(1, HIVE_NULL_ACCOUNT, 1) when absent). Go's
// chain.RequiredAuthorities does not yet expose the "other_auths"
// equivalent — porting it requires extending the API to return a
// []auth.Authority slice alongside the per-level account sets, and
// threading that into sign_state for key-direct signature matching.
func TestWitnessSetPropertiesAuthoritiesKeyAuthMissing(t *testing.T) {
	t.Skip("MISSING: chain.RequiredAuthorities does not expose per-op " +
		"`other_auths` (key-authority) — needed for the witness_set_properties " +
		"`key` prop path that signs against a specific public_key rather " +
		"than against an account_name. Extending RequiredAuthorities + " +
		"sign_state to carry []auth.Authority unlocks this assertion.")
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

// TestWitnessUpdateAuthorities ports
// operation_tests.cpp::witness_update_authorities. Exercises the
// no-sig / wrong-key / duplicate / irrelevant / success / signing-key
// (witness BlockSigningKey ≠ active authority) paths against
// witness_update (active authority).
func TestWitnessUpdateAuthorities(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.FundLiquid("alice", 10_000)
	f.GenerateBlock()

	aliceActive := f.ActorKey("alice")
	alicePosting := f.ActorPostingKey("alice")
	bobActive := f.ActorKey("bob")

	// The witness's block_signing_key — a deterministic key independent
	// of alice's account authority. The C++ test verifies that even
	// though this is the witness's public signing identity, it is NOT
	// part of alice's active authority and so cannot sign witness_update.
	signingKey := derivePrivateKey(t, "new_key")
	signingKeyBytes := signingKey.PublicKey()

	body, unsigned := buildTxBody(f, op.WitnessUpdate{
		Owner:           "alice",
		URL:             "foo.bar",
		BlockSigningKey: signingKeyBytes,
		Props: op.LegacyChainProperties{
			AccountCreationFee: LegacyHIVE(1_000),
			MaximumBlockSize:   65536,
			HBDInterestRate:    1000,
		},
		Fee: HIVE(1_000),
	})

	t.Log("--- Test failure when no signatures (tx_missing_active_auth)")
	requireMissingActive(t, f, body, unsigned)

	t.Log("--- Test failure when signed by posting key (tx_missing_active_auth)")
	stxPost := signTx(t, f, body, unsigned, alicePosting)
	requireVerifyErr(t, f, &stxPost, unsigned, "active", false)

	t.Log("--- Test failure when duplicate signatures (tx_duplicate_sig)")
	stxDup := signTx(t, f, body, unsigned, aliceActive, aliceActive)
	requireDuplicateSig(t, f, &stxDup, unsigned)

	t.Log("--- Test failure with additional bob signature (tx_irrelevant_sig)")
	stxIrrel := signTx(t, f, body, unsigned, aliceActive, bobActive)
	requireIrrelevantSig(t, f, &stxIrrel, unsigned)

	t.Log("--- Test success with alice's active signature")
	stxOK := signTx(t, f, body, unsigned, aliceActive)
	if err := chain.VerifyAuthority(state.NewSession(f.State), &stxOK, unsigned, network.OldChainID, false); err != nil {
		t.Fatalf("active-signed witness_update should verify: %v", err)
	}

	t.Log("--- Test failure when signed only by the witness's own block_signing_key (tx_missing_active_auth)")
	// signing_key is NOT in alice's active authority — the witness's
	// block_signing_key is consensus identity for block signing, not
	// for active-tx signing. Must fail with missing-active.
	stxSigning := signTx(t, f, body, unsigned, signingKey)
	requireVerifyErr(t, f, &stxSigning, unsigned, "active", false)
}

// ---------- helpers ----------

// singleOpTx wraps a single op.Operation in a SignedTransaction (no
// signatures, no header values — used purely to feed
// chain.RequiredAuthorities for structural-only authorities tests).
func singleOpTx(o op.Operation) tx.SignedTransaction {
	return tx.SignedTransaction{
		Transaction: tx.Transaction{
			Operations: []op.Op{op.FromOperation(o)},
		},
	}
}

// requireSetEqual compares chain.RequiredAuthorities's per-level map
// against the expected sorted slice of account names. The map is the
// in-Go representation of hived's flat_set<account_name_type>.
func requireSetEqual(t *testing.T, label string, got map[string]struct{}, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %d names, want %d (got=%v want=%v)", label, len(got), len(want), keysSorted(got), want)
		return
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("%s: missing %q (got=%v)", label, name, keysSorted(got))
		}
	}
}

func keysSorted(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// buildTxBody returns (body, unsignedBytes) for a transaction
// containing exactly `o`. The unsigned-byte form is used by
// chain.VerifyAuthority's digest computation.
func buildTxBody(f *Fixture, o op.Operation) (tx.Transaction, []byte) {
	body := tx.Transaction{
		RefBlockNum:    uint16(f.BlockNum & 0xFFFF),
		RefBlockPrefix: 0,
		Expiration:     f.HeadBlockTime() + 60,
		Operations:     []op.Op{op.FromOperation(o)},
	}
	unsigned := tx.AppendTransactionBody(nil, &body)
	return body, unsigned
}

// signTx returns a SignedTransaction with one signature per key. The
// digest is sha256(network.OldChainID || unsignedBytes) — pre-HF24
// signing format; all test fixtures default to that since
// HardforksUpTo defaults to genesis.
func signTx(t *testing.T, f *Fixture, body tx.Transaction, unsigned []byte, keys ...*crypto.PrivateKey) tx.SignedTransaction {
	t.Helper()
	_ = f
	chainID := network.OldChainID
	digest := crypto.SHA256Of2(chainID[:], unsigned)
	stx := tx.SignedTransaction{Transaction: body}
	for _, k := range keys {
		sig, err := crypto.SignCompact(k, digest)
		if err != nil {
			t.Fatalf("signTx: SignCompact: %v", err)
		}
		stx.Signatures = append(stx.Signatures, sig)
	}
	return stx
}

// requireMissingActive asserts that a 0-signature tx fails with a
// "missing active authority" error — mirrors C++'s tx_missing_active_auth.
func requireMissingActive(t *testing.T, f *Fixture, body tx.Transaction, unsigned []byte) {
	t.Helper()
	stx := tx.SignedTransaction{Transaction: body}
	requireVerifyErr(t, f, &stx, unsigned, "active", false)
}

// requireDuplicateSig asserts the sig recovery surfaces ErrDuplicateSig.
func requireDuplicateSig(t *testing.T, f *Fixture, stx *tx.SignedTransaction, unsigned []byte) {
	t.Helper()
	err := chain.VerifyAuthority(state.NewSession(f.State), stx, unsigned, network.OldChainID, false)
	if err == nil {
		t.Fatal("expected duplicate-sig rejection, got nil")
	}
	if !errors.Is(err, chain.ErrDuplicateSig) {
		t.Fatalf("expected ErrDuplicateSig, got: %v", err)
	}
}

// requireIrrelevantSig asserts the authority walk surfaces ErrIrrelevantSig.
func requireIrrelevantSig(t *testing.T, f *Fixture, stx *tx.SignedTransaction, unsigned []byte) {
	t.Helper()
	err := chain.VerifyAuthority(state.NewSession(f.State), stx, unsigned, network.OldChainID, false)
	if err == nil {
		t.Fatal("expected irrelevant-sig rejection, got nil")
	}
	if !errors.Is(err, chain.ErrIrrelevantSig) {
		t.Fatalf("expected ErrIrrelevantSig, got: %v", err)
	}
}

// requireVerifyErr asserts that VerifyAuthority returns an error whose
// message contains `wantSubstr` (case-insensitive). When `wantSuccess`
// is true, the call must succeed instead.
func requireVerifyErr(t *testing.T, f *Fixture, stx *tx.SignedTransaction, unsigned []byte, wantSubstr string, wantSuccess bool) {
	t.Helper()
	err := chain.VerifyAuthority(state.NewSession(f.State), stx, unsigned, network.OldChainID, false)
	if wantSuccess {
		if err != nil {
			t.Fatalf("expected verify success, got: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", wantSubstr)
	}
	if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(wantSubstr)) {
		t.Fatalf("expected error containing %q, got: %v", wantSubstr, err)
	}
}
