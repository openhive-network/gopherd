package chain

import (
	"math/big"

	"hived-go/internal/protocol/hardfork"
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

// HIVE_APR_PERCENT_MULTIPLY_PER_BLOCK / SHIFT_PER_BLOCK from
// libraries/protocol/include/hive/protocol/config.hpp:305. The
// compounded per-block multiplier is
// int(expm1( log1p( 1 ) / BLOCKS_PER_YEAR ) * 2**87 / 100000 + 0.5).
// Formula: reward = (supply × percent × multiply + half) >> shift.
const (
	aprMultiplyPerBlock uint64 = 0x5ccc_e802_de5f
	aprShiftPerBlock    uint   = 87
	aprMultiplyPerRound uint64 = 0x79cc_f5c7_3480
	aprShiftPerRound    uint   = 83
	aprMultiplyPerHour  uint64 = 0x6cc1_39a1_5cbd
	aprShiftPerHour     uint   = 77
)

// percentRewardPerBlock mirrors hived's `calc_percent_reward_per_block<
// percent >( virtual_supply )` (compound.hpp:38).  The compounded fixed-
// point formula stays accurate at every supply, where a linear
// approximation diverges enough to push past the floor a few hundred
// thousand blocks before C++ would.
func percentRewardPerBlock(virtualSupply, aprPercent int64) int64 {
	return percentRewardCompound(virtualSupply, aprPercent, aprMultiplyPerBlock, aprShiftPerBlock)
}

func percentRewardPerRound(virtualSupply, aprPercent int64) int64 {
	return percentRewardCompound(virtualSupply, aprPercent, aprMultiplyPerRound, aprShiftPerRound)
}

// percentRewardPerHour mirrors hived's calc_percent_reward_per_hour
// (compound.hpp:32) — used by the liquidity reward subsystem which fires
// once per hour.
func percentRewardPerHour(virtualSupply, aprPercent int64) int64 {
	return percentRewardCompound(virtualSupply, aprPercent, aprMultiplyPerHour, aprShiftPerHour)
}

// applyWitnessRewardWeighting takes the per-block witness slice and
// applies the C++ witness_reward * 21 * schedule_weight / pay_norm
// formula. Falls back to the unweighted slice if the schedule object
// or this witness aren't loadable (early chain or stub state).
func applyWitnessRewardWeighting(s *state.Session, witnessReward int64, witnessName string) int64 {
	wso, err := objects.GetWitnessSchedule(s)
	if err != nil {
		return witnessReward
	}
	w, err := objects.GetWitness(s, witnessName)
	if err != nil {
		return witnessReward
	}
	const maxWitnesses int64 = 21
	weighted := witnessReward * maxWitnesses
	switch w.Schedule {
	case objects.WitnessScheduleTimeshare:
		weighted *= int64(wso.TimeshareWeight)
	case objects.WitnessScheduleMiner:
		weighted *= int64(wso.MinerWeight)
	case objects.WitnessScheduleElected:
		weighted *= int64(wso.ElectedWeight)
	default:
		// Schedule == none means the witness isn't on the active
		// schedule — but they produced this block, so something's
		// off. Fall back to the unweighted slice to avoid 0-pay
		// surprises during the early HF16 transition.
		return witnessReward
	}
	if wso.WitnessPayNormalizationFactor == 0 {
		return witnessReward
	}
	return weighted / int64(wso.WitnessPayNormalizationFactor)
}

func percentRewardCompound(virtualSupply, aprPercent int64, multiplyConstant uint64, shiftConstant uint) int64 {
	if virtualSupply <= 0 || aprPercent <= 0 {
		return 0
	}
	// reward = ((supply × percent × multiply) + half) >> shift, in uint128.
	prod := new(big.Int).Mul(big.NewInt(virtualSupply), big.NewInt(aprPercent))
	prod.Mul(prod, new(big.Int).SetUint64(multiplyConstant))
	half := new(big.Int).Lsh(big.NewInt(1), shiftConstant-1)
	prod.Add(prod, half)
	prod.Rsh(prod, shiftConstant)
	if !prod.IsInt64() {
		return int64(^uint64(0) >> 1) // saturate
	}
	return prod.Int64()
}

// processProducerReward credits the producing witness 1 HIVE per block.
// Pre-startMinerVotingBlock the reward is liquid (balance); after, it
// goes to vesting_shares.
//
// HF16+: this is a no-op. process_funds (HF16+ branch) computes and
// pays the witness's inflation slice as VESTS in one step; calling
// this would double-credit. Mirrors hived's database.cpp where
// get_producer_reward only runs in the pre-HF16 branch.
func processProducerReward(s *state.Session, witnessName string, blockNum uint32) {
	if witnessName == "" {
		return
	}
	hf := hardfork.SetForBlock(blockNum)
	if hf.Has(hardfork.HF16) {
		return
	}
	_ = hf
	a, err := objects.GetAccount(s, witnessName)
	if err != nil {
		// Witness account hasn't been created yet — nothing to credit.
		return
	}
	pay := minProducerReward
	// Hived: VESTS path if head_block >= 864000 OR witness has 0 vesting.
	// We DON'T bump current_supply here — C++ adds witness_pay to
	// current_supply inside process_funds's modify block (matching the
	// content/vesting modifications). Adding it twice in two places
	// races the GHIVEDIFF dump timing; instead processFunds owns the
	// supply increment so the dump (between this and processFunds)
	// observes pre-modify state matching C++.
	asVests := blockNum >= startMinerVotingBlock || a.VestingShares == 0

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
func processFunds(s *state.Session, blockNum uint32, witnessName string) {
	hf := hardfork.SetForBlock(blockNum)
	dgpo, err := objects.GetDGPO(s)
	if err != nil && err != state.ErrNotFound {
		return
	}

	if hf.Has(hardfork.HF16) {
		processFundsHF16(s, &dgpo, blockNum, witnessName)
		return
	}
	// Pre-HF16 path: rewards computed off virtual_supply (HIVE + HBD-as-HIVE
	// at the median feed). C++ database.cpp:2331 calls update_virtual_supply
	// just before process_funds, so by the time get_content_reward fires
	// virtual_supply already reflects HBD growth. Using current_supply here
	// produces under-rewards once HBD circulates (pre-HF1) and once a feed
	// is published.
	virtualSupply := dgpo.VirtualSupply
	if virtualSupply <= 0 {
		virtualSupply = dgpo.CurrentSupply
	}

	contentReward := percentRewardPerBlock(virtualSupply, contentAPRPercent)
	if contentReward < minContentReward {
		contentReward = minContentReward
	}
	curateReward := percentRewardPerBlock(virtualSupply, curateAPRPercent)
	if curateReward < minCurateReward {
		curateReward = minCurateReward
	}
	witnessPay := minProducerReward

	vestingReward := contentReward + curateReward + witnessPay
	if blockNum < startVestingBlock {
		vestingReward = 0
	} else {
		vestingReward *= 9
	}

	dgpo.TotalVestingFundHive += vestingReward
	dgpo.TotalRewardFundHive += contentReward + curateReward
	// Mirror C++ database.cpp:1909:
	//   p.current_supply += content_reward + witness_pay + vesting_reward
	// (where content_reward is the curate-folded value).
	dgpo.CurrentSupply += contentReward + curateReward + witnessPay + vestingReward
	objects.PutDGPO(s, &dgpo)
}

// HIVE_INFLATION constants from libraries/protocol/include/hive/protocol/config.hpp.
const (
	inflationStartBP   int64 = 978     // 9.78 %
	inflationStopBP    int64 = 95      // 0.95 %
	inflationNarrowing int64 = 250_000 // narrows by 1bp every 250k blocks
)

// processFundsHF16 mirrors database::process_funds (post-HF16 branch,
// libraries/chain/database.cpp:1710-1793). New HIVE per block is
//
//	current_inflation_rate = max(start - block/250000, stop)            // basis points
//	new_hive               = virtual_supply × current_inflation_rate
//	                       / (HIVE_100_PERCENT × HIVE_BLOCKS_PER_YEAR)
//
// Sliced as content × content_reward_percent + vesting × vesting_reward_percent
// + dhf × proposal_fund_percent + witness_reward = remainder.
//
// Witness reward is paid as VESTS (post-block-864000 we always
// create_vesting). Pre-HF17 the content slice goes into
// dgpo.total_reward_fund_hive; HF17+ it lands in the post reward_fund.
//
// We elide the witness_pay_normalization_factor / schedule weighting
// (timeshare/miner/elected) since the witness schedule object is not
// modeled — we just apply the content/vesting/proposal/witness split
// directly.
func processFundsHF16(s *state.Session, dgpo *objects.DynamicGlobalProperty, blockNum uint32, witnessName string) {
	const blocksPerYear int64 = 365 * 24 * 60 * 60 / 3
	rate := inflationStartBP - int64(blockNum)/inflationNarrowing
	if rate < inflationStopBP {
		rate = inflationStopBP
	}
	virtualSupply := dgpo.VirtualSupply
	if virtualSupply <= 0 {
		virtualSupply = dgpo.CurrentSupply
	}

	// new_hive = virtual_supply × rate / (10000 × blocks_per_year). C++
	// (database.cpp:1738) does this with HIVE_asset arithmetic that
	// internally widens the multiply to uint128 — the
	// `virtualSupply / 10000` form below truncates the bottom 4 digits
	// of the supply before the rate multiply, losing up to ~rate satoshi
	// per block (cumulating to thousands of satoshis over a full
	// replay). math/big keeps the full precision.
	prodBig := new(big.Int).Mul(big.NewInt(virtualSupply), big.NewInt(rate))
	prodBig.Quo(prodBig, big.NewInt(10000*blocksPerYear))
	if prodBig.Sign() < 0 {
		prodBig.SetInt64(0)
	}
	newHive := int64(0)
	if prodBig.IsInt64() {
		newHive = prodBig.Int64()
	}

	contentPct := int64(dgpo.ContentRewardPercent)
	if contentPct == 0 {
		contentPct = 7500
	}
	vestingPct := int64(dgpo.VestingRewardPercent)
	if vestingPct == 0 {
		vestingPct = 1500
	}
	dhfPct := int64(dgpo.ProposalFundPercent)

	contentReward := newHive * contentPct / 10000
	vestingReward := newHive * vestingPct / 10000
	dhfNew := newHive * dhfPct / 10000
	witnessReward := newHive - contentReward - vestingReward - dhfNew
	if witnessReward < 0 {
		witnessReward = 0
	}

	// Mirror C++ process_funds HF16+ branch (database.cpp:1748-1763):
	//
	//	witness_reward *= HIVE_MAX_WITNESSES   // 21
	//	if witness.schedule == timeshare: witness_reward *= wso.timeshare_weight
	//	elif schedule == miner:          witness_reward *= wso.miner_weight
	//	elif schedule == elected:        witness_reward *= wso.elected_weight
	//	witness_reward /= wso.witness_pay_normalization_factor
	//
	// Without the schedule weighting, every block's producer gets a
	// flat 1/21 of the inflation slice — i.e. ~21× too low at HF16+.
	if witnessReward > 0 && witnessName != "" {
		witnessReward = applyWitnessRewardWeighting(s, witnessReward, witnessName)
	}

	hf := hardfork.SetForBlock(blockNum)
	if hf.Has(hardfork.HF17) {
		// HF17+: content_reward goes into post reward_fund.
		// HF21+: dgpo.downvote_pool_percent slice is carved into the
		// fund's downvote_reward_balance instead. Mirrors C++
		// process_funds (database.cpp) at HF21.
		if fund, err := objects.GetRewardFund(s, objects.PostRewardFundName); err == nil {
			upvoteShare := contentReward
			downvoteShare := int64(0)
			if hf.Has(hardfork.HF21) && dgpo.DownvotePoolPercent > 0 {
				// reward_split_pct = ((100% - downvote_pool_percent) ×
				// content_reward + 5000) / 100% — round-half-up.
				num := int64(hundredPercentBP) - int64(dgpo.DownvotePoolPercent)
				upvoteShare = (num*contentReward + 5000) / int64(hundredPercentBP)
				downvoteShare = contentReward - upvoteShare
			}
			fund.RewardBalance += upvoteShare
			fund.DownvoteRewardBalance += downvoteShare
			objects.PutRewardFund(s, &fund)
		}
	} else {
		dgpo.TotalRewardFundHive += contentReward
	}

	dgpo.TotalVestingFundHive += vestingReward
	dgpo.CurrentSupply += contentReward + vestingReward + witnessReward + dhfNew
	if dhfNew > 0 {
		// Treasury gets the DHF allocation as HBD-equivalent. Without
		// a feed we credit it as HIVE; once the feed is wired hived
		// converts via median_price. Pre-HF24 treasury is "steem.dao";
		// HF24+ "hive.fund". We try the post-HF24 name first.
		if treasury, err := objects.GetAccount(s, "hive.fund"); err == nil {
			treasury.Balance += dhfNew
			objects.PutAccount(s, &treasury)
		} else if treasury, err := objects.GetAccount(s, "steem.dao"); err == nil {
			treasury.Balance += dhfNew
			objects.PutAccount(s, &treasury)
		}
	}
	objects.PutDGPO(s, dgpo)

	// Witness reward payable as VESTS via create_vesting at the dynamic
	// price. Post-HF16 hived's process_funds is the SOLE producer-pay
	// path (we skip processProducerReward in that branch). The witness
	// is the producer of THIS block (passed in via witnessName).
	if witnessReward > 0 && witnessName != "" {
		_, _ = CreateVesting_via_chain(s, witnessName, witnessReward, false)
	}
}
