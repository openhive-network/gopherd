package chain

import (
	"hived-go/internal/protocol/auth"
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
	contentAPRPercent  int64 = 3875 // 38.75%
	curateAPRPercent   int64 = 3875
	producerAPRPercent int64 = 750 // 7.5%
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
//
// KNOWN LATENT BUG (tracked as task #22): when the producer's
// witness.Schedule field doesn't match what hived's schedule had for
// that slot — e.g. because gopherd's schedule rebuild diverged due to
// account_name_type ordering (task #21) or the missing median fields
// (task #24) — this function multiplies by the WRONG weight (5× or
// 1/5×). The chain still pays the correct producer (hdr.Witness), but
// by the wrong amount. Detection: instrument by dumping per-block
// (witness, schedule_field, expected_slot_type) to a TSV and grep for
// divergences. Fix path: pass the EXPECTED slot's schedule type
// (looked up via ws.ScheduledWitnessForSlot at applyBlock time) into
// this function rather than reading the witness's stored Schedule.
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
	// reward = ((supply × percent × multiply) + half) >> shift, in
	// uint128 (mirrors the int128 calc_percent_reward path in
	// libraries/protocol/include/hive/protocol/misc_utilities.hpp).
	// Native Uint128 path replaces the math/big.Int chain (3 heap
	// allocations + 1 Lsh) that was 5%+ of replay alloc volume.
	supply := objects.Uint128{Lo: uint64(virtualSupply)}
	prod := supply.Mul64(uint64(aprPercent))
	prod = prod.Mul64(multiplyConstant)
	// half = 1 << (shiftConstant - 1)
	var half objects.Uint128
	if shiftConstant >= 1 && shiftConstant <= 128 {
		if shiftConstant-1 < 64 {
			half = objects.Uint128{Lo: 1 << (shiftConstant - 1)}
		} else {
			half = objects.Uint128{Hi: 1 << (shiftConstant - 1 - 64)}
		}
	}
	prod = prod.Add(half)
	// >> shiftConstant
	if shiftConstant >= 128 {
		return 0
	}
	if shiftConstant >= 64 {
		prod = objects.Uint128{Lo: prod.Hi >> (shiftConstant - 64)}
	} else if shiftConstant > 0 {
		prod = objects.Uint128{
			Hi: prod.Hi >> shiftConstant,
			Lo: (prod.Lo >> shiftConstant) | (prod.Hi << (64 - shiftConstant)),
		}
	}
	if prod.Hi != 0 || prod.Lo > uint64(int64(^uint64(0)>>1)) {
		return int64(^uint64(0) >> 1) // saturate
	}
	return int64(prod.Lo)
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
	// Witness existence + VestingShares read — read-only, use Ref to
	// skip the per-block Account value-copy (one of the higher-volume
	// GetAccount paths since it fires every block).
	witnessRef, ok := objects.GetAccountRef(s, auth.AccountName(witnessName))
	if !ok {
		return // Witness account hasn't been created yet — nothing to credit.
	}
	// hived: pay = max(virtual_supply × 7.5% / blocks_per_year, MIN).
	// Until virtual_supply grows past ~140M HIVE the formula stays at the
	// floor; after that (~block 1.7M) C++ pays a percent-of-supply slice
	// and creates that many VESTS at vest_price. Treating the formula as
	// a flat MIN under-credits the witness by hundreds of sat-HIVE per
	// block, drifting TVF/TVS at the rate of compounding inflation.
	dgpoRef, dgpoOk := objects.GetDGPORef(s)
	pay := minProducerReward
	if dgpoOk {
		virtualSupply := dgpoRef.VirtualSupply
		if virtualSupply <= 0 {
			virtualSupply = dgpoRef.CurrentSupply
		}
		percent := percentRewardPerBlock(virtualSupply, producerAPRPercent)
		if percent > pay {
			pay = percent
		}
	}
	// Hived: VESTS path if head_block >= 864000 OR witness has 0 vesting.
	// We DON'T bump current_supply here — C++ adds witness_pay to
	// current_supply inside process_funds's modify block (matching the
	// content/vesting modifications). Adding it twice in two places
	// races the GHIVEDIFF dump timing; instead processFunds owns the
	// supply increment so the dump (between this and processFunds)
	// observes pre-modify state matching C++.
	asVests := blockNum >= startMinerVotingBlock || witnessRef.VestingShares == 0

	if asVests {
		// CreateVesting moves `pay` HIVE into the vesting fund, computes
		// VESTS at the dynamic price, and credits the witness.
		if _, err := CreateVesting_via_chain(s, witnessName, pay, false); err != nil {
			return
		}
	} else {
		// Liquid-pay path: in-place mutation via MutateAccount avoids
		// the Account value-copy on Get + the copy-back on Put.
		objects.MutateAccount(s, auth.AccountName(witnessName), func(a *objects.Account) {
			a.Balance += pay
		})
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
	if hf.Has(hardfork.HF16) {
		// HF16+ branches off into processFundsHF16, which still uses
		// the GetDGPO+PutDGPO pattern (more complex flow). Pre-HF16
		// can use MutateDGPO since the entire body is read-modify-write
		// on the same slot.
		dgpo, err := objects.GetDGPO(s)
		if err != nil && err != state.ErrNotFound {
			return
		}
		processFundsHF16(s, &dgpo, blockNum, witnessName)
		return
	}
	// Pre-HF16 path: rewards computed off virtual_supply (HIVE + HBD-as-HIVE
	// at the median feed). MutateDGPO collapses read+write into a single
	// in-place slot mutation; no value-copies on the way in or out.
	objects.MutateDGPO(s, func(p *objects.DynamicGlobalProperty) {
		virtualSupply := p.VirtualSupply
		if virtualSupply <= 0 {
			virtualSupply = p.CurrentSupply
		}
		contentReward := percentRewardPerBlock(virtualSupply, contentAPRPercent)
		if contentReward < minContentReward {
			contentReward = minContentReward
		}
		curateReward := percentRewardPerBlock(virtualSupply, curateAPRPercent)
		if curateReward < minCurateReward {
			curateReward = minCurateReward
		}
		witnessPay := percentRewardPerBlock(virtualSupply, producerAPRPercent)
		if witnessPay < minProducerReward {
			witnessPay = minProducerReward
		}
		vestingReward := contentReward + curateReward + witnessPay
		if blockNum < startVestingBlock {
			vestingReward = 0
		} else {
			vestingReward *= 9
		}
		p.TotalVestingFundHive += vestingReward
		p.TotalRewardFundHive += contentReward + curateReward
		delta := contentReward + curateReward + witnessPay + vestingReward
		p.CurrentSupply += delta
		p.VirtualSupply += delta
	})
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

	// HF28+ ONLY: use virtual_supply_without_treasury for new_hive
	// inflation. hived gates this on HIVE_HARDFORK_1_28_NO_DHF_HBD_IN_INFLATION
	// (= HIVE_HARDFORK_1_28; database.cpp:1741), NOT HF21. Treasury-held HBD
	// is excluded from the inflation base so the dhf budget doesn't compound
	// on its own holdings. Pre-HF28 the FULL virtual_supply is correct —
	// gating this at HF21 shrank the inflation base by treasury HBD every
	// block from HF21 on, drifting content_reward 1 sat/block low once the
	// treasury's accumulated DHF HBD grew enough to tip newHive's floor
	// (first observable at block 35,932,133; starved the reward fund and
	// left likwid's claim_reward_balance 1,982,263 reward-VESTS short at
	// 35,933,333).
	hfInfl := hardfork.SetForBlock(blockNum)
	if hfInfl.Has(hardfork.HF28) {
		if treasury, terr := objects.GetAccount(s, currentTreasuryName(s, hfInfl)); terr == nil && treasury.HBDBalance > 0 {
			if fh, ferr := objects.GetFeedHistory(s); ferr == nil && !priceIsZero(fh.CurrentMedianHistory) {
				treasuryAsHive := convertHBDToHive(treasury.HBDBalance, fh.CurrentMedianHistory)
				if treasuryAsHive > 0 && treasuryAsHive < virtualSupply {
					virtualSupply -= treasuryAsHive
				}
			}
		}
	}

	// new_hive = virtual_supply × rate / (10000 × blocks_per_year). C++
	// (database.cpp:1738) does this with HIVE_asset arithmetic that
	// internally widens the multiply to uint128 — the
	// `virtualSupply / 10000` form below truncates the bottom 4 digits
	// of the supply before the rate multiply, losing up to ~rate satoshi
	// per block (cumulating to thousands of satoshis over a full
	// replay). Native Uint128 keeps full precision without a big.Int
	// alloc per per-block call.
	newHive := int64(0)
	if virtualSupply > 0 && rate > 0 {
		prod := objects.Uint128{Lo: uint64(virtualSupply)}.Mul64(uint64(rate))
		divisor := uint64(10000) * uint64(blocksPerYear)
		quo := prod.Quo64(divisor)
		if quo.Hi == 0 && quo.Lo <= uint64(int64(^uint64(0)>>1)) {
			newHive = int64(quo.Lo)
		} else {
			newHive = int64(^uint64(0) >> 1)
		}
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
		// HF17+: the FULL content_reward goes into the post reward_fund's
		// reward_balance (C++ pay_reward_funds, database.cpp:1949-1970 —
		// reward_balance += content_reward × percent_content_rewards/100%,
		// =100% for the single post fund). hived has NO reward-fund
		// upvote/downvote split: the HF21 "downvote pool" is purely a
		// VOTING-MANA concept (downvote_manabar), not a carve of the
		// reward fund (grep confirms no downvote_reward_balance anywhere
		// in hived). Go previously diverted downvote_pool_percent (25% at
		// HF21) of content_reward into a phantom DownvoteRewardBalance,
		// starving the cashout fund by ~one block's 25% every block from
		// HF21 on — drifting every post-HF21 cashout (a-bot claim short
		// at block 35,921,981; reward_balance 464 sat low one block after
		// HF21, accumulating).
		if fund, err := objects.GetRewardFund(s, objects.PostRewardFundName); err == nil {
			fund.RewardBalance += contentReward
			objects.PutRewardFund(s, &fund)
		}
	} else {
		dgpo.TotalRewardFundHive += contentReward
	}

	dgpo.TotalVestingFundHive += vestingReward
	// Mirror C++ database.cpp:1767-1784. The HIVE side is straightforward:
	//   current_supply += new_hive   (= contentReward + vestingReward + witnessReward)
	//   virtual_supply += new_hive + dhf_new_funds
	// The DHF (HF21+) side: dhf_new_funds is HIVE that gets CONVERTED to HBD
	// via the median price and credited to the treasury as HBD. Pre-HF24
	// treasury is "steem.dao"; HF24+ "hive.fund".
	newHiveDelta := contentReward + vestingReward + witnessReward
	dgpo.CurrentSupply += newHiveDelta
	dgpo.VirtualSupply += newHiveDelta + dhfNew
	if dhfNew > 0 {
		// Convert dhfNew HIVE → HBD via median feed for the treasury.
		var dhfHBD int64
		if fh, ferr := objects.GetFeedHistory(s); ferr == nil && !priceIsZero(fh.CurrentMedianHistory) {
			dhfHBD = convertHiveToHBD(dhfNew, fh.CurrentMedianHistory)
		}
		// Resolve the treasury by the HF24-gated name. MUST NOT probe
		// "hive.fund" first: it existed as a user account from 2020-03-18
		// (pre-HF24), so a hive.fund-first probe would route DHF inflation to
		// that user account instead of the real pre-HF24 treasury steem.dao.
		treasuryName := currentTreasuryName(s, hf)
		if dhfHBD <= 0 {
			// Fall back to crediting HIVE if the feed isn't available.
			if treasury, err := objects.GetAccount(s, treasuryName); err == nil {
				treasury.Balance += dhfNew
				objects.PutAccount(s, &treasury)
				dgpo.CurrentSupply += dhfNew
			}
		} else {
			if treasury, err := objects.GetAccount(s, treasuryName); err == nil {
				// Mirror hived process_funds (database_comment.cpp:
				// adjust_balance( get_treasury_name(), hbd_issued )): route the
				// per-block DHF HBD add through the hbd_seconds/interest accrual
				// against THIS block's dgpo copy. A direct `treasury.HBDBalance
				// += dhfHBD` skipped the accrual, so the treasury's hbd_seconds
				// was over-counted at the hourly proposal-payout AdjustHBDBalance
				// calls (end-of-hour balance charged for the whole hour) and its
				// HBD interest over-paid — hbd_supply drifted +1151 at the
				// hive.fund interest payout (block 52,107,629), cascading through
				// virtual_supply → new_hive → reward_balance → curation pool →
				// pending vesting → leo.voter claim_reward_balance fail @52,378,769.
				objects.AdjustHBDBalanceInto(s, &treasury, dgpo.Time, dhfHBD, dgpo)
				objects.PutAccount(s, &treasury)
			}
			dgpo.CurrentHBDSupply += dhfHBD
			dgpo.DHFIntervalLedger += dhfHBD
		}
	}
	objects.PutDGPO(s, dgpo)

	// Witness reward payable as VESTS via create_vesting at the dynamic
	// price. Post-HF16 hived's process_funds is the SOLE producer-pay
	// path (we skip processProducerReward in that branch). The witness
	// is the producer of THIS block (passed in via witnessName).
	if witnessReward > 0 && witnessName != "" {
		if _, err := CreateVesting_via_chain(s, witnessName, witnessReward, false); err != nil {
			// Silent failure here historically dropped the per-block witness
			// VESTS credit while leaving CurrentSupply / TotalVestingFundHive
			// already incremented above — the source of the steem-account
			// +301k VESTS drift cluster in blocks 12014697-12019198. Surface
			// it so any future divergence is observable in logs.
			ghiveDiff("GHIVEDIFF CREATE-VESTING-FAIL site=funds.processFunds block=%d witness=%s amount=%d err=%v\n",
				blockNum, witnessName, witnessReward, err)
		}
	}
}
