package evaluator

import (
	"math"
	"math/bits"

	"gopherd/internal/state/objects"
)

// rcAdjustmentFromFee converts an account-creation fee in HIVE to VESTS at the
// current vesting share price, mirroring hived
// (hive_evaluator_account.cpp:159-164):
//
//	rc_adjustment_from_fee = ( fee * dgpo.get_vesting_share_price() ).amount.value
//
// This value becomes the new account's rc_adjustment, which is what lets an
// account with no VESTS transact at all. Pre-HF20 it is zero — those accounts
// get their RC fields from the HF20 initialisation loop instead.
func rcAdjustmentFromFee(dgpo *objects.DynamicGlobalProperty, feeHive int64) int64 {
	if feeHive <= 0 {
		return 0
	}
	priceVests := dgpo.TotalVestingShares
	priceFund := dgpo.TotalVestingFundHive
	// Same 1:1000 genesis fallback the vesting path uses when either total is
	// zero; without it an early-chain division would panic.
	if priceFund == 0 || priceVests == 0 {
		return feeHive * 1000
	}
	hi, lo := bits.Mul64(uint64(feeHive), uint64(priceVests))
	if hi >= uint64(priceFund) {
		return math.MaxInt64
	}
	q, _ := bits.Div64(hi, lo, uint64(priceFund))
	if q > uint64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(q)
}
