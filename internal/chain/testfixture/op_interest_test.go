package testfixture

import (
	"math/big"
	"testing"

	"hived-go/internal/protocol/op"
)

// HIVE_HBD_INTEREST_COMPOUND_INTERVAL_SEC = 30 days.
const hbdInterestCompoundIntervalSec uint32 = 60 * 60 * 24 * 30

// expectedInterest computes the C++ formula
//
//	interest = (hbd × seconds / SECONDS_PER_YEAR) * rate / 10000
//
// using the same division order so int128 truncation matches.
func expectedInterest(hbd int64, seconds uint32, rateBP int64) int64 {
	const secondsPerYear = int64(60 * 60 * 24 * 365)
	cur := new(big.Int).Mul(big.NewInt(hbd), big.NewInt(int64(seconds)))
	cur.Quo(cur, big.NewInt(secondsPerYear))
	cur.Mul(cur, big.NewInt(rateBP))
	cur.Quo(cur, big.NewInt(10000))
	if !cur.IsInt64() {
		return 0
	}
	return cur.Int64()
}

// Port of operation_time_tests.cpp::hbd_interest (pre-HF25 path).
//
// alice receives 31.903 HBD, then waits one full compounding interval.
// A subsequent transfer triggers AdjustHBDBalance which pays out the
// accrued interest on alice's liquid HBD. The new balance must equal
// (start - 1.000) + interest computed via the chain formula.
//
// We use the FAST SkipSeconds path because HBD interest accrues
// lazily — the formula is `hbd × (block_time - last_update)`, not
// per-block accumulation, so jumping the clock and walking one block
// at the end produces the same result as walking every block.
func TestHBDInterestPayoutPreHF25(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()
	f.FundLiquidHBD("alice", 31_903)

	preBlockTime := f.HeadBlockTime()
	startUpdate := f.Account_("alice").HBDSecondsLastUpdate
	if startUpdate == 0 || startUpdate > preBlockTime {
		t.Fatalf("alice.HBDSecondsLastUpdate not initialized: %d (head=%d)",
			startUpdate, preBlockTime)
	}
	aliceHBD := f.HBDBalance("alice")
	if aliceHBD != 31_903 {
		t.Fatalf("alice initial HBD: got %d, want 31903", aliceHBD)
	}

	// Skip one full compounding interval (30 days).
	f.SkipSeconds(hbdInterestCompoundIntervalSec + BlockInterval)

	// Transfer 1.000 HBD → triggers AdjustHBDBalance on alice.
	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "bob", Amount: HBD(1_000),
	}); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	// Compute expected interest for the elapsed window (start_time → now).
	rate := int64(f.DGPO().HBDInterestRate)
	if rate == 0 {
		rate = 1000 // default 10%
	}
	now := f.HeadBlockTime()
	elapsed := now - startUpdate
	want := aliceHBD - 1_000 + expectedInterest(aliceHBD, elapsed, rate)
	got := f.HBDBalance("alice")
	if got != want {
		t.Fatalf("alice HBD after interest: got %d, want %d (elapsed=%d, rate=%d)",
			got, elapsed, want, rate)
	}
}

// Port of operation_time_tests.cpp::hbd_savings_interest.
//
// alice deposits 31.903 HBD into savings; after one compounding
// interval, a self-transfer-to-savings triggers AdjustSavingsHBDBalance
// which pays out savings interest. (Savings interest is paid both
// pre- and post-HF25.)
func TestHBDSavingsInterestPayout(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.GenerateBlock()

	// Seed alice with 31.903 HBD on savings + 3.000 HBD liquid.
	f.FundLiquidHBD("alice", 31_903)
	if err := f.ApplyOp(op.TransferToSavings{
		From: "alice", To: "alice", Amount: HBD(31_903),
	}); err != nil {
		t.Fatalf("transfer_to_savings: %v", err)
	}
	f.FundLiquidHBD("alice", 3_000)

	preSavings := f.SavingsHBDBalance("alice")
	if preSavings != 31_903 {
		t.Fatalf("alice savings HBD: got %d, want 31903", preSavings)
	}
	startUpdate := f.Account_("alice").SavingsHBDSecondsLastUpdate

	// Skip one compounding interval.
	f.SkipSeconds(hbdInterestCompoundIntervalSec + BlockInterval)

	// Trigger interest payout via another savings deposit.
	if err := f.ApplyOp(op.TransferToSavings{
		From: "alice", To: "alice", Amount: HBD(1_000),
		Memo: "interest trigger",
	}); err != nil {
		t.Fatalf("transfer_to_savings 2: %v", err)
	}

	rate := int64(f.DGPO().HBDInterestRate)
	if rate == 0 {
		rate = 1000
	}
	now := f.HeadBlockTime()
	elapsed := now - startUpdate
	wantInterest := expectedInterest(preSavings, elapsed, rate)
	want := preSavings + 1_000 + wantInterest
	got := f.SavingsHBDBalance("alice")
	if got != want {
		t.Fatalf("alice savings HBD after interest: got %d, want %d (interest=%d)",
			got, want, wantInterest)
	}
}

// Ports the third sub-test of operation_time_tests.cpp::
// hbd_savings_interest ("Testing savings interest for longer period").
// After the first payout, a sub-interval deposit accumulates coindays
// without paying; a subsequent 7/3 × 30-day skip + deposit pays
// interest computed over (current window + carried coindays).
func TestHBDSavingsInterestLongerPeriodWithCoindays(t *testing.T) {
	f := New(t)
	f.Actor("alice")
	f.GenerateBlock()

	f.FundLiquidHBD("alice", 31_903)
	if err := f.ApplyOp(op.TransferToSavings{
		From: "alice", To: "alice", Amount: HBD(31_903),
	}); err != nil {
		t.Fatalf("seed savings: %v", err)
	}
	// Need a stock of liquid HBD to fund the trigger deposits.
	f.FundLiquidHBD("alice", 10_000)

	// Warmup: 30 days + savings deposit → interest fires.
	f.SkipSeconds(hbdInterestCompoundIntervalSec + BlockInterval)
	if err := f.ApplyOp(op.TransferToSavings{
		From: "alice", To: "alice", Amount: HBD(1_000),
	}); err != nil {
		t.Fatalf("warmup deposit: %v", err)
	}

	// Sub-interval deposit (15d) — carry coindays, no payout.
	startUpdate := f.Account_("alice").SavingsHBDSecondsLastUpdate
	aliceSav := f.SavingsHBDBalance("alice")
	f.SkipSeconds(hbdInterestCompoundIntervalSec / 2)
	if err := f.ApplyOp(op.TransferToSavings{
		From: "alice", To: "alice", Amount: HBD(1_000),
	}); err != nil {
		t.Fatalf("sub-interval deposit: %v", err)
	}
	coindays := uint64(aliceSav) * uint64(f.HeadBlockTime()-startUpdate)
	aliceSavPostSub := f.SavingsHBDBalance("alice")
	startUpdate2 := f.Account_("alice").SavingsHBDSecondsLastUpdate

	// Longer interval (7/3 of compound interval) + trigger op.
	f.SkipSeconds(hbdInterestCompoundIntervalSec * 7 / 3)
	if err := f.ApplyOp(op.TransferToSavings{
		From: "alice", To: "alice", Amount: HBD(1_000),
	}); err != nil {
		t.Fatalf("long deposit: %v", err)
	}

	rate := int64(f.DGPO().HBDInterestRate)
	if rate == 0 {
		rate = 1000
	}
	now := f.HeadBlockTime()
	elapsed := now - startUpdate2
	totalCoindays := uint64(aliceSavPostSub)*uint64(elapsed) + coindays
	const secondsPerYear = uint64(60 * 60 * 24 * 365)
	interest := int64(totalCoindays/secondsPerYear) * rate / 10000
	want := aliceSavPostSub + 1_000 + interest
	got := f.SavingsHBDBalance("alice")
	// Allow ±1 sat for truncation order.
	if got != want && got != want-1 && got != want+1 {
		t.Fatalf("alice savings HBD after long interest: got %d, want %d (interest=%d, elapsed=%d, coindays=%d)",
			got, want, interest, elapsed, totalCoindays)
	}
}

// Ports the third sub-test of operation_time_tests.cpp::hbd_interest
// ("Testing longer interest period"). After the initial 30-day window
// pays out and a sub-interval transfer accumulates coindays without
// paying, a subsequent 7/3 × 30-day skip + transfer pays interest
// computed over (current window + carried coindays). Coverage gap vs
// existing TestHBDInterestNoPayoutBelowInterval which only validates
// the no-payout middle segment.
func TestHBDInterestLongerPeriodWithCoindays(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()
	f.FundLiquidHBD("alice", 31_903)

	// Warmup: 30 days + transfer → interest fires, last_payment refreshed.
	f.SkipSeconds(hbdInterestCompoundIntervalSec + BlockInterval)
	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "bob", Amount: HBD(1_000),
	}); err != nil {
		t.Fatalf("warmup transfer: %v", err)
	}

	// Sub-interval transfer (15d) — accrues coindays, no payout.
	startUpdate := f.Account_("alice").HBDSecondsLastUpdate
	aliceHBD := f.HBDBalance("alice")
	f.SkipSeconds(hbdInterestCompoundIntervalSec / 2)
	subTransferStart := f.HeadBlockTime()
	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "bob", Amount: HBD(1_000),
	}); err != nil {
		t.Fatalf("sub-interval transfer: %v", err)
	}
	// Carry: (alice_hbd × elapsed_to_sub) is captured in hbd_seconds.
	coindays := uint64(aliceHBD) * uint64(f.HeadBlockTime()-startUpdate)
	_ = subTransferStart

	// Now skip a longer window (7/3 of compound interval ≈ 70 days) and
	// transfer once more. The combined accrual covers the carried
	// coindays plus the new window.
	aliceHBDPostSub := f.HBDBalance("alice")
	startUpdate2 := f.Account_("alice").HBDSecondsLastUpdate
	f.SkipSeconds(hbdInterestCompoundIntervalSec * 7 / 3)
	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "bob", Amount: HBD(1_000),
	}); err != nil {
		t.Fatalf("long transfer: %v", err)
	}

	rate := int64(f.DGPO().HBDInterestRate)
	if rate == 0 {
		rate = 1000
	}
	now := f.HeadBlockTime()
	elapsed := now - startUpdate2
	totalCoindays := uint64(aliceHBDPostSub)*uint64(elapsed) + coindays
	// interest = totalCoindays / seconds_per_year * rate / HIVE_100_PERCENT
	const secondsPerYear = uint64(60 * 60 * 24 * 365)
	interest := int64(totalCoindays/secondsPerYear) * rate / 10000
	want := aliceHBDPostSub - 1_000 + interest
	got := f.HBDBalance("alice")
	// Allow ±1 sat for ordering differences in the big-int truncation.
	if got != want && got != want-1 && got != want+1 {
		t.Fatalf("alice HBD after long interest: got %d, want %d (interest=%d, elapsed=%d, coindays=%d)",
			got, want, interest, elapsed, totalCoindays)
	}
}

// Sub-interval transfers should NOT pay interest — only accumulate
// hbd_seconds. Mirrors the second half of operation_time_tests.cpp::
// hbd_interest: after the first 30-day window has paid out (which
// updates hbd_last_interest_payment to a recent value), a subsequent
// transfer at 15 days transfers without paying interest.
func TestHBDInterestNoPayoutBelowInterval(t *testing.T) {
	f := New(t)
	f.Actors("alice", "bob")
	f.GenerateBlock()
	f.FundLiquidHBD("alice", 31_903)

	// Warmup: run one full 30-day window so hbd_last_interest_payment
	// gets set to a recent time. After this transfer, alice has
	// (31903 - 1000 + interest) and last_interest_payment = head_time.
	f.SkipSeconds(hbdInterestCompoundIntervalSec + BlockInterval)
	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "bob", Amount: HBD(1_000),
	}); err != nil {
		t.Fatalf("warmup transfer: %v", err)
	}
	aliceHBD := f.HBDBalance("alice")

	// Now skip half the compounding interval (15 days) and transfer
	// again — should NOT pay interest.
	f.SkipSeconds(hbdInterestCompoundIntervalSec / 2)
	if err := f.ApplyOp(op.Transfer{
		From: "alice", To: "bob", Amount: HBD(1_000),
	}); err != nil {
		t.Fatalf("sub-interval transfer: %v", err)
	}

	want := aliceHBD - 1_000
	got := f.HBDBalance("alice")
	if got != want {
		t.Fatalf("alice HBD (sub-interval): got %d, want %d", got, want)
	}
}
