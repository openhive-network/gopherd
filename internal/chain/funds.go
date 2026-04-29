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
	if blockNum < startMinerVotingBlock {
		a.Balance += pay
	} else {
		// HIVE → VESTS conversion: HIVE has 3 decimals, VESTS has 6,
		// so HIVE-satoshi × 1000 = VESTS-satoshi at the 1:1 price.
		a.VestingShares += pay * 1000
	}
	objects.PutAccount(s, &a)

	dgpo, err := objects.GetDGPO(s)
	if err != nil && err != state.ErrNotFound {
		return
	}
	if blockNum < startMinerVotingBlock {
		dgpo.CurrentSupply += pay
	} else {
		dgpo.TotalVestingShares += pay * 1000
	}
	objects.PutDGPO(s, &dgpo)
}
