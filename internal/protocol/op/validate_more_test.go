package op

import (
	"testing"

	"gopherd/internal/protocol/asset"
)

// CPP-parity validate tests ported from
// libraries/chain/tests/operation_tests.cpp.

// expectErr asserts that v.Validate() returns an error.
func expectErr(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: expected validation error, got nil", label)
	}
}

func expectOK(t *testing.T, label string, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: unexpected error: %v", label, err)
	}
}

// price builds an asset.Price from base/quote shorthand.
func priceOf(base, quote asset.Asset) asset.Price {
	return asset.Price{Base: base, Quote: quote}
}

// ---- WithdrawVesting ---------------------------------------------------

func TestWithdrawVestingValidate(t *testing.T) {
	expectOK(t, "valid", WithdrawVesting{Account: "alice", VestingShares: vest(1000)}.Validate())
	expectErr(t, "wrong symbol", WithdrawVesting{Account: "alice", VestingShares: hive(1000)}.Validate())
	expectErr(t, "bad name", WithdrawVesting{Account: "X", VestingShares: vest(1000)}.Validate())
}

// ---- AccountUpdate / AccountUpdate2 -----------------------------------

func TestAccountUpdateValidate(t *testing.T) {
	expectOK(t, "valid", AccountUpdate{Account: "alice"}.Validate())
	expectErr(t, "bad name", AccountUpdate{Account: "X"}.Validate())
}

func TestAccountUpdate2Validate(t *testing.T) {
	expectOK(t, "valid", AccountUpdate2{Account: "alice"}.Validate())
	expectErr(t, "bad name", AccountUpdate2{Account: "X"}.Validate())
}

// ---- Convert / CollateralizedConvert ----------------------------------

func TestConvertValidate(t *testing.T) {
	expectOK(t, "valid HBD→HIVE", Convert{Owner: "alice", RequestID: 1, Amount: hbd(100)}.Validate())
	expectErr(t, "wrong symbol", Convert{Owner: "alice", Amount: hive(100)}.Validate())
	expectErr(t, "zero", Convert{Owner: "alice", Amount: hbd(0)}.Validate())
}

func TestCollateralizedConvertValidate(t *testing.T) {
	expectOK(t, "valid HIVE→HBD", CollateralizedConvert{Owner: "alice", Amount: hive(100)}.Validate())
	expectErr(t, "wrong symbol", CollateralizedConvert{Owner: "alice", Amount: hbd(100)}.Validate())
	expectErr(t, "zero", CollateralizedConvert{Owner: "alice", Amount: hive(0)}.Validate())
}

// ---- FeedPublish ------------------------------------------------------

// TestFeedPublishValidate ports operation_tests.cpp::feed_publish_validate.
// Per hive_operations.cpp the price must be HIVE/HBD or HBD/HIVE in either
// orientation; positive amounts; valid publisher.
func TestFeedPublishValidate(t *testing.T) {
	good := FeedPublish{Publisher: "alice", ExchangeRate: priceOf(hive(1000), hbd(1000))}
	expectOK(t, "valid", good.Validate())
	swapped := FeedPublish{Publisher: "alice", ExchangeRate: priceOf(hbd(1000), hive(1000))}
	expectOK(t, "valid swapped", swapped.Validate())
	bad := FeedPublish{Publisher: "alice", ExchangeRate: priceOf(hive(1000), hive(1000))}
	expectErr(t, "both HIVE", bad.Validate())
}

// ---- LimitOrderCancel / LimitOrderCreate2 -----------------------------

func TestLimitOrderCancelValidate(t *testing.T) {
	expectOK(t, "valid", LimitOrderCancel{Owner: "alice", OrderID: 1}.Validate())
	expectErr(t, "bad name", LimitOrderCancel{Owner: "X"}.Validate())
}

// ---- Escrow validate suite -------------------------------------------

// TestEscrowTransferValidate mirrors the C++ table-driven escrow_transfer_validate.
func TestEscrowTransferValidate(t *testing.T) {
	base := EscrowTransfer{
		From: "alice", To: "bob", Agent: "sam", EscrowID: 0,
		HbdAmount: hbd(1000), HiveAmount: hive(1000), Fee: hive(100),
		RatificationDeadline: 100, EscrowExpiration: 200,
	}
	expectOK(t, "valid", base.Validate())

	// hbd symbol != HBD
	bad := base
	bad.HbdAmount = hive(1000)
	expectErr(t, "hbd not HBD", bad.Validate())

	// hive symbol != HIVE
	bad = base
	bad.HiveAmount = hbd(1000)
	expectErr(t, "hive not HIVE", bad.Validate())

	// fee not HIVE/HBD
	bad = base
	bad.Fee = vest(100)
	expectErr(t, "fee bad symbol", bad.Validate())

	// both amounts zero
	bad = base
	bad.HbdAmount = hbd(0)
	bad.HiveAmount = hive(0)
	expectErr(t, "both zero", bad.Validate())

	// negative hbd
	bad = base
	bad.HbdAmount = hbd(-100)
	expectErr(t, "neg hbd", bad.Validate())

	// negative hive
	bad = base
	bad.HiveAmount = hive(-100)
	expectErr(t, "neg hive", bad.Validate())

	// negative fee
	bad = base
	bad.Fee = hive(-100)
	expectErr(t, "neg fee", bad.Validate())

	// ratification == expiration
	bad = base
	bad.RatificationDeadline = base.EscrowExpiration
	expectErr(t, "ratification == expiration", bad.Validate())

	// ratification > expiration
	bad = base
	bad.RatificationDeadline = base.EscrowExpiration + 100
	expectErr(t, "ratification > expiration", bad.Validate())

	// agent == from
	bad = base
	bad.Agent = base.From
	expectErr(t, "agent == from", bad.Validate())
}

func TestEscrowApproveValidate(t *testing.T) {
	expectOK(t, "valid agent", EscrowApprove{From: "ali", To: "bob", Agent: "sam", Who: "sam"}.Validate())
	expectOK(t, "valid to", EscrowApprove{From: "ali", To: "bob", Agent: "sam", Who: "bob"}.Validate())
	expectErr(t, "who=from invalid", EscrowApprove{From: "ali", To: "bob", Agent: "sam", Who: "ali"}.Validate())
}

func TestEscrowDisputeValidate(t *testing.T) {
	expectOK(t, "valid from", EscrowDispute{From: "ali", To: "bob", Agent: "sam", Who: "ali"}.Validate())
	expectOK(t, "valid to", EscrowDispute{From: "ali", To: "bob", Agent: "sam", Who: "bob"}.Validate())
	expectErr(t, "who=agent invalid", EscrowDispute{From: "ali", To: "bob", Agent: "sam", Who: "sam"}.Validate())
}

func TestEscrowReleaseValidate(t *testing.T) {
	good := EscrowRelease{
		From: "ali", To: "bob", Agent: "sam", Who: "ali", Receiver: "bob",
		HbdAmount: hbd(100), HiveAmount: hive(100),
	}
	expectOK(t, "valid", good.Validate())
	bad := good
	bad.Receiver = bad.Agent
	expectErr(t, "receiver=agent invalid", bad.Validate())
	bad = good
	bad.HbdAmount = hbd(0)
	bad.HiveAmount = hive(0)
	expectErr(t, "zero amounts", bad.Validate())
}

// ---- Savings family ---------------------------------------------------

func TestTransferToSavingsValidate(t *testing.T) {
	expectOK(t, "valid HIVE", TransferToSavings{From: "ali", To: "bob", Amount: hive(100)}.Validate())
	expectOK(t, "valid HBD", TransferToSavings{From: "ali", To: "bob", Amount: hbd(100)}.Validate())
	expectErr(t, "VESTS rejected", TransferToSavings{From: "ali", To: "bob", Amount: vest(100)}.Validate())
	expectErr(t, "zero", TransferToSavings{From: "ali", To: "bob", Amount: hive(0)}.Validate())
}

func TestTransferFromSavingsValidate(t *testing.T) {
	expectOK(t, "valid HIVE", TransferFromSavings{From: "ali", To: "bob", Amount: hive(100)}.Validate())
	expectOK(t, "valid HBD", TransferFromSavings{From: "ali", To: "bob", Amount: hbd(100)}.Validate())
	expectErr(t, "VESTS rejected", TransferFromSavings{From: "ali", To: "bob", Amount: vest(100)}.Validate())
}

func TestCancelTransferFromSavingsValidate(t *testing.T) {
	expectOK(t, "valid", CancelTransferFromSavings{From: "alice"}.Validate())
	expectErr(t, "bad name", CancelTransferFromSavings{From: "X"}.Validate())
}

// ---- ClaimAccount ----------------------------------------------------

// TestClaimAccountValidate ports operation_tests.cpp::claim_account_validate.
func TestClaimAccountValidate(t *testing.T) {
	expectOK(t, "zero fee", ClaimAccount{Creator: "alice", Fee: hive(0)}.Validate())
	expectOK(t, "small fee", ClaimAccount{Creator: "alice", Fee: hive(100)}.Validate())
	expectErr(t, "wrong symbol", ClaimAccount{Creator: "alice", Fee: hbd(100)}.Validate())
	expectErr(t, "negative fee", ClaimAccount{Creator: "alice", Fee: hive(-1)}.Validate())
	expectErr(t, "fee too large",
		ClaimAccount{Creator: "alice", Fee: hive(maxAccountCreationFeeSatoshi + 1)}.Validate())
}

func TestCreateClaimedAccountValidate(t *testing.T) {
	expectOK(t, "valid", CreateClaimedAccount{Creator: "alice", NewAccountName: "bob"}.Validate())
	expectErr(t, "bad creator", CreateClaimedAccount{Creator: "X", NewAccountName: "bob"}.Validate())
}

func TestAccountCreateWithDelegationValidate(t *testing.T) {
	good := AccountCreateWithDelegation{
		Creator: "alice", NewAccountName: "bob",
		Fee: hive(100), Delegation: vest(10000),
	}
	expectOK(t, "valid", good.Validate())
	bad := good
	bad.Fee = hbd(100)
	expectErr(t, "fee not HIVE", bad.Validate())
	bad = good
	bad.Delegation = hive(10000)
	expectErr(t, "delegation not VESTS", bad.Validate())
}

// ---- ClaimRewardBalance ----------------------------------------------

func TestClaimRewardBalanceValidate(t *testing.T) {
	expectOK(t, "claim hive only",
		ClaimRewardBalance{Account: "alice", RewardHive: hive(100), RewardHbd: hbd(0), RewardVests: vest(0)}.Validate())
	expectErr(t, "all zero",
		ClaimRewardBalance{Account: "alice", RewardHive: hive(0), RewardHbd: hbd(0), RewardVests: vest(0)}.Validate())
	expectErr(t, "wrong hive symbol",
		ClaimRewardBalance{Account: "alice", RewardHive: hbd(100), RewardHbd: hbd(0), RewardVests: vest(0)}.Validate())
	expectErr(t, "negative",
		ClaimRewardBalance{Account: "alice", RewardHive: hive(-1), RewardHbd: hbd(0), RewardVests: vest(0)}.Validate())
}

// ---- DelegateVestingShares -------------------------------------------

func TestDelegateVestingSharesValidate(t *testing.T) {
	expectOK(t, "valid",
		DelegateVestingShares{Delegator: "alice", Delegatee: "bob", VestingShares: vest(10000)}.Validate())
	expectErr(t, "self-delegation",
		DelegateVestingShares{Delegator: "alice", Delegatee: "alice", VestingShares: vest(10000)}.Validate())
	expectErr(t, "wrong symbol",
		DelegateVestingShares{Delegator: "alice", Delegatee: "bob", VestingShares: hive(10000)}.Validate())
	expectErr(t, "negative",
		DelegateVestingShares{Delegator: "alice", Delegatee: "bob", VestingShares: vest(-1)}.Validate())
}

// ---- WitnessSetProperties --------------------------------------------

func TestWitnessSetPropertiesValidate(t *testing.T) {
	expectOK(t, "with key",
		WitnessSetProperties{Owner: "alice", Props: []WitnessProperty{{Key: "key", Value: []byte{0}}}}.Validate())
	expectErr(t, "missing key",
		WitnessSetProperties{Owner: "alice", Props: []WitnessProperty{{Key: "url", Value: []byte("u")}}}.Validate())
}

// ---- CommentOptions / SetWithdrawVestingRoute ------------------------

func TestCommentOptionsValidate(t *testing.T) {
	good := CommentOptions{
		Author: "alice", Permlink: "p",
		MaxAcceptedPayout: hbd(1_000_000), PercentHbd: 5000,
	}
	expectOK(t, "valid", good.Validate())
	bad := good
	bad.PercentHbd = 10001
	expectErr(t, "percent > 100%", bad.Validate())
	bad = good
	bad.MaxAcceptedPayout = hive(100)
	expectErr(t, "wrong symbol", bad.Validate())
	bad = good
	bad.MaxAcceptedPayout = hbd(-1)
	expectErr(t, "negative payout", bad.Validate())
}

// TestCommentBeneficiariesValidate mirrors the C++ comment_beneficiaries_validate
// test (libraries/chain/tests/operation_tests.cpp). Covers:
//   - single beneficiary > 100% weight
//   - cumulative beneficiaries > 100%
//   - up to 127 routes allowed; 128+ rejected (HIVE_BENEFICIARY_LIMIT - 1)
//   - duplicate / out-of-order accounts rejected
func TestCommentBeneficiariesValidate(t *testing.T) {
	base := func(bs []CommentBeneficiary) CommentOptions {
		return CommentOptions{
			Author: "alice", Permlink: "test",
			MaxAcceptedPayout: hbd(1_000_000), PercentHbd: 5000,
			Beneficiaries: bs,
		}
	}
	expectErr(t, "single > 100%",
		base([]CommentBeneficiary{{Account: "bob", Weight: hivePercent + 1}}).Validate())
	expectErr(t, "cumulative > 100%",
		base([]CommentBeneficiary{
			{Account: "bob", Weight: 7500},
			{Account: "sam", Weight: 7500},
		}).Validate())

	// 127 routes (max) — generate sorted names foo0..foo126
	max127 := make([]CommentBeneficiary, 127)
	for i := range max127 {
		max127[i] = CommentBeneficiary{Account: routeAcct(i), Weight: 1}
	}
	expectOK(t, "127 routes max", base(max127).Validate())
	expectErr(t, "128 routes too many",
		base(append(max127, CommentBeneficiary{Account: "zzzz", Weight: 1})).Validate())

	expectErr(t, "duplicate accounts",
		base([]CommentBeneficiary{
			{Account: "bob", Weight: 200},
			{Account: "bob", Weight: 100},
		}).Validate())

	expectErr(t, "unsorted accounts",
		base([]CommentBeneficiary{
			{Account: "bob", Weight: 100},
			{Account: "alice", Weight: 100},
		}).Validate())
}

// routeAcct generates a 6-char zero-padded account "ben000".."ben999"
// — sortable lexicographically, at least HIVE_MIN_ACCOUNT_NAME_LENGTH (3).
func routeAcct(i int) string {
	s := []byte{'0', '0', '0'}
	for d := 2; d >= 0 && i > 0; d-- {
		s[d] = byte('0' + i%10)
		i /= 10
	}
	return "ben" + string(s)
}

func TestSetWithdrawVestingRouteValidate(t *testing.T) {
	expectOK(t, "valid",
		SetWithdrawVestingRoute{FromAccount: "alice", ToAccount: "bob", Percent: 5000}.Validate())
	expectErr(t, "percent > 100%",
		SetWithdrawVestingRoute{FromAccount: "alice", ToAccount: "bob", Percent: 10001}.Validate())
}

// ---- RecurrentTransfer -----------------------------------------------

func TestRecurrentTransferValidate(t *testing.T) {
	good := RecurrentTransfer{
		From: "alice", To: "bob", Amount: hive(100),
		Recurrence: 24, Executions: 5,
	}
	expectOK(t, "valid", good.Validate())
	bad := good
	bad.Recurrence = 1
	expectErr(t, "recurrence too low", bad.Validate())
	bad = good
	bad.Amount = vest(100)
	expectErr(t, "VESTS amount", bad.Validate())
	bad = good
	bad.To = bad.From
	expectErr(t, "to == from", bad.Validate())
	bad = good
	bad.Executions = 1
	expectErr(t, "fewer than 2 executions", bad.Validate())
}

// ---- Pow / Pow2 ------------------------------------------------------

func TestPowValidate(t *testing.T) {
	expectOK(t, "valid", Pow{WorkerAccount: "alice"}.Validate())
	expectErr(t, "bad name", Pow{WorkerAccount: "X"}.Validate())
}

func TestPow2Validate(t *testing.T) {
	expectOK(t, "valid",
		Pow2{WorkVariantTag: 0, PowInput: Pow2Input{WorkerAccount: "alice"}}.Validate())
	expectErr(t, "bad name",
		Pow2{WorkVariantTag: 0, PowInput: Pow2Input{WorkerAccount: "X"}}.Validate())
}
