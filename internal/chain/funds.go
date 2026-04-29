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

// HIVE_MIN_CONTENT_REWARD / HIVE_MIN_CURATE_REWARD — both default to
// 1 HIVE-satoshi (= HIVE_MINING_REWARD) per block at the floor.
const (
	minContentReward int64 = 1000
	minCurateReward  int64 = 1000
)

// HIVE_*_APR_PERCENT — basis-points-of-100% APR figures from
// libraries/protocol/include/hive/protocol/config.hpp.
const (
	contentAPRPercent int64 = 3875 // 38.75%
	curateAPRPercent  int64 = 3875
)

// HIVE_BLOCKS_PER_YEAR = 365 × 24 × 60 × 60 / 3.
const hiveBlocksPerYear int64 = 365 * 24 * 60 * 60 / 3

// HIVE_START_VESTING_BLOCK = 7 days × 28,800 = 201,600. Before this
// block, the per-block vesting_reward inflation is zero; after, it
// becomes 9 × (content + curate + witness) and grows total_vesting_fund_hive
// directly.
const startVestingBlock uint32 = 201_600

// percentRewardPerBlock approximates hived's
// `protocol::calc_percent_reward_per_block< APR_PCT >( virtual_supply )` —
// the per-block reward at the given APR basis-points-of-100%. The C++
// implementation uses a compounded fixed-point formula; the linear
// approximation `supply * APR / 10000 / blocks_per_year` is within a
// small percent of it for the supply ranges we care about.
func percentRewardPerBlock(virtualSupply, aprPercent int64) int64 {
	if virtualSupply <= 0 {
		return 0
	}
	return virtualSupply * aprPercent / 10000 / hiveBlocksPerYear
}

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

// processFunds models hived's database::process_funds for the pre-HF16
// path (libraries/chain/database.cpp:1796–1816):
//
//	content_reward = HIVE_MIN_CONTENT_REWARD
//	curate_reward  = HIVE_MIN_CURATE_REWARD
//	witness_pay    = HIVE_MIN_PRODUCER_REWARD                  (already paid)
//	vesting_reward = (content + curate + witness)
//	if head < HIVE_START_VESTING_BLOCK: vesting_reward = 0
//	else                              : vesting_reward *= 9
//	dgpo.total_vesting_fund_hive += vesting_reward
//	dgpo.current_supply          += content_reward + witness_pay + vesting_reward
//
// We omit total_reward_fund_hive (the HIVE-side bucket the comment
// cashout pipeline draws from) since cashout's HIVE/VESTS payouts are
// currently funded directly by CreateVesting; once reward_fund_hive
// tracking lands processCommentCashouts will subtract from it instead.
//
// The witness_pay leg of current_supply is added inside
// processProducerReward, so we only contribute content_reward +
// vesting_reward here. This keeps the supply ledger consistent with
// hived's per-block delta.
func processFunds(s *state.Session, blockNum uint32) {
	dgpo, err := objects.GetDGPO(s)
	if err != nil && err != state.ErrNotFound {
		return
	}
	// virtual_supply = current_supply + (HBD valued in HIVE). Until we
	// track the price feed, approximate it as current_supply.
	virtualSupply := dgpo.CurrentSupply

	contentReward := percentRewardPerBlock(virtualSupply, contentAPRPercent)
	if contentReward < minContentReward {
		contentReward = minContentReward
	}
	curateReward := percentRewardPerBlock(virtualSupply, curateAPRPercent)
	if curateReward < minCurateReward {
		curateReward = minCurateReward
	}
	witnessPay := minProducerReward // already paid in processProducerReward

	vestingReward := contentReward + curateReward + witnessPay
	if blockNum < startVestingBlock {
		vestingReward = 0
	} else {
		vestingReward *= 9
	}

	dgpo.TotalVestingFundHive += vestingReward
	// hived: total_reward_fund_hive grows by the per-block content_reward;
	// processCommentCashouts later draws from this fund to pay authors
	// and curators.
	dgpo.TotalRewardFundHive += contentReward + curateReward
	// content_reward + vesting_reward; witness_pay is added in
	// processProducerReward to avoid double-counting.
	dgpo.CurrentSupply += contentReward + curateReward + vestingReward
	objects.PutDGPO(s, &dgpo)
}
