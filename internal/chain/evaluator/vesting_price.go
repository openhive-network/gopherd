package evaluator

// HiveToVestsRatio is the conversion factor used by create_vesting at
// the early 1:1 vesting price (1 HIVE = 1 VESTS, where HIVE has 3 decimal
// places and VESTS has 6, so 1 HIVE-satoshi = 1000 VESTS-satoshi).
//
// Hived's actual conversion uses dynamic price:
//
//	new_vesting_shares = hive_amount * total_vesting_shares / total_vesting_fund_hive
//
// The ratio diverges from 1:1 over time as VESTS accumulate from
// curation/producer rewards while HIVE is destroyed/created at a
// different rate. Tracking total_vesting_fund_hive (a separate DGPO
// field) is a follow-up; for now this constant captures the early-
// chain ratio precisely and is the dominant correct value through
// the first few hardforks.
const HiveToVestsRatio int64 = 1000
