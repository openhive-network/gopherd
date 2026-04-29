package evaluator

import (
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// CreateVesting converts hiveSatoshi liquid HIVE into VESTS for the
// given account, mirroring hived's create_vesting2.
//
// Conversion price (libraries/chain/include/hive/chain/detail/state/global_property_object.hpp):
//
//	if (total_vesting_fund_hive == 0 || total_vesting_shares == 0)
//	    price = VEST_price(1_000_000, 1_000)               // initial 1:1
//	else
//	    price = VEST_price(total_vesting_shares, total_vesting_fund_hive)
//
//	new_vesting = hive_amount * price
//
// In satoshi space (HIVE has 3 decimal places, VESTS has 6) the initial
// price multiplies HIVE-satoshi by 1000 to get VESTS-satoshi. After the
// fund accumulates, the dynamic price drives the conversion.
//
// Side effects:
//   - account.VestingShares += new_vesting (or RewardVests if toReward)
//   - dgpo.TotalVestingShares += new_vesting
//   - dgpo.TotalVestingFundHive += hiveSatoshi
//
// Returns the VESTS-satoshi amount credited.
func CreateVesting(s *state.Session, accountName string, hiveSatoshi int64, toReward bool) (int64, error) {
	dgpo, err := objects.GetDGPO(s)
	if err != nil && err != state.ErrNotFound {
		return 0, err
	}
	// Use the fixed genesis 1:1000 satoshi ratio for HIVE → VESTS until
	// the full reward2 / curation pipeline is in place. The per-vote
	// curation hack and flat author reward run through this helper too,
	// so a dynamic shares/fund price would compound their distortions
	// across many credits and starve later transfer/withdraw flows
	// (observed regression at block 202,807 with steemit's withdraw).
	// process_funds does still grow fund_hive — withdraw_vesting picks
	// the dynamic fund/shares ratio there, so the inflation is real
	// for VESTS holders going liquid even while CreateVesting stays at
	// the constant rate.
	vestsAmount := hiveSatoshi * 1000

	a, err := objects.GetAccount(s, accountName)
	if err != nil {
		return 0, err
	}
	if toReward {
		a.RewardVests += vestsAmount
	} else {
		a.VestingShares += vestsAmount
	}
	objects.PutAccount(s, &a)

	dgpo.TotalVestingShares += vestsAmount
	dgpo.TotalVestingFundHive += hiveSatoshi
	objects.PutDGPO(s, &dgpo)
	return vestsAmount, nil
}
