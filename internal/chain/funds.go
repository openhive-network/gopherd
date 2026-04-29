package chain

import (
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_START_MINER_VOTING_BLOCK = 30 days × HIVE_BLOCKS_PER_DAY (28,800).
// Before this block the producer reward is paid as liquid HIVE balance;
// after, it's paid as VESTS via create_vesting (we approximate by
// crediting the witness's VestingShares directly since vesting price
// tracking is deferred).
const startMinerVotingBlock uint32 = 864_000

// HIVE_MIN_PRODUCER_REWARD — the producer reward floor in HIVE satoshi.
// `asset(1000, HIVE_SYMBOL)` per libraries/protocol/include/hive/protocol/config.hpp.
// hived's full formula is
//
//	pay = max(virtual_supply.amount * HIVE_PRODUCER_APR_PERCENT
//	          / HIVE_100_PERCENT / HIVE_BLOCKS_PER_YEAR,
//	          HIVE_MIN_PRODUCER_REWARD)
//
// which scales pay up with chain inflation. We use the floor as a
// minimum-viable approximation: every block credits 1 HIVE to the
// producing witness and ticks the global supply by the same amount.
// Refining to the proper APR formula requires virtual_supply tracking.
const minProducerReward int64 = 1000

// processProducerReward credits the producing witness 1 HIVE per block.
// Pre-startMinerVotingBlock the reward is liquid (balance); after, it
// goes to vesting_shares.
func processProducerReward(s *state.Session, witnessName string, blockNum uint32) {
	if witnessName == "" {
		return
	}
	a, err := objects.GetAccount(s, witnessName)
	if err != nil {
		// Witness account hasn't been created yet — nothing to credit.
		return
	}
	pay := minProducerReward
	// Hived: VESTS path if head_block >= 864000 OR witness has 0 vesting.
	// hived's adjust_supply runs before either branch, so dgpo.current_supply
	// always rises by `pay`. Inside the VESTS path, create_vesting moves
	// `pay` HIVE into total_vesting_fund_hive and credits the resulting
	// VESTS to the witness.
	asVests := blockNum >= startMinerVotingBlock || a.VestingShares == 0

	dgpo, err := objects.GetDGPO(s)
	if err != nil && err != state.ErrNotFound {
		return
	}
	dgpo.CurrentSupply += pay
	objects.PutDGPO(s, &dgpo)

	if asVests {
		// CreateVesting moves `pay` HIVE into the vesting fund, computes
		// VESTS at the dynamic price, and credits the witness.
		if _, err := CreateVesting_via_chain(s, witnessName, pay, false); err != nil {
			return
		}
	} else {
		// Refresh `a` since CurrentSupply update might have happened in
		// a different branch — but here we only need to mutate Balance.
		a, err := objects.GetAccount(s, witnessName)
		if err != nil {
			return
		}
		a.Balance += pay
		objects.PutAccount(s, &a)
	}
}
