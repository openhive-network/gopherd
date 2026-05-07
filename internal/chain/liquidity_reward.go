package chain

import (
	"fmt"
	"os"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_LIQUIDITY_REWARD_BLOCKS — once per hour (60×60 / 3-second blocks).
const liquidityRewardBlocks uint32 = 60 * 60 / 3

// HIVE_MIN_LIQUIDITY_REWARD = 1000 satoshi × HIVE_LIQUIDITY_REWARD_BLOCKS,
// i.e. 1200 HIVE per hour at the floor (1 HIVE/block × 1200 blocks).
const liquidityRewardMin int64 = 1000 * int64(liquidityRewardBlocks)

// payLiquidityReward mirrors database::pay_liquidity_reward in
// libraries/chain/database_vesting.cpp:29. Once per
// HIVE_LIQUIDITY_REWARD_BLOCKS:
//
//   1. Compute reward = max(virtual_supply × HIVE_LIQUIDITY_APR_PERCENT
//      / 100% / HIVE_BLOCKS_PER_HOUR, HIVE_MIN_LIQUIDITY_REWARD).
//   2. Find the highest-weight liquidity_reward_balance row.
//   3. If its weight > 0:
//        adjust_supply(+reward) — mints new HIVE.
//        adjust_balance(owner, +reward) — credits the LP.
//        Reset row's volumes/weight to 0; refresh last_update.
//
// HF12__178 disables the entire liquidity-reward subsystem.
func payLiquidityReward(s *state.Session, blockNum, blockTime uint32) {
	if blockNum%liquidityRewardBlocks != 0 {
		return
	}
	hf := hardfork.SetForBlock(blockNum)
	if hf.Has(hardfork.HF12) {
		return
	}
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return
	}
	// C++ database.cpp:1877 uses props.virtual_supply.amount (HBD value
	// included via the median feed). Pre-feed they're equal; post-feed
	// virtual_supply > current_supply, so using the latter under-rewards
	// LPs by the HBD share. The floor (HIVE_MIN_LIQUIDITY_REWARD) usually
	// dominates pre-HF12, so this rarely affects payouts in practice.
	supplyForLPCalc := dgpo.VirtualSupply
	if supplyForLPCalc <= 0 {
		supplyForLPCalc = dgpo.CurrentSupply
	}
	reward := liquidityRewardForVirtualSupply(supplyForLPCalc)
	if reward <= 0 {
		return
	}

	// Find the highest-weight row by walking the by-weight secondary
	// index in REVERSE (it's sorted ascending; the last entry has the
	// largest weight). We collect the last visited candidate during
	// forward iteration.
	type candidate struct {
		weight [16]byte
		owner  string
	}
	var best candidate
	_ = s.Range(objects.LiquidityRewardBalanceTable, objects.LiquidityRewardByWeightIndex, nil,
		func(body, _ []byte) (bool, error) {
			if len(body) < 16+state.AccountNameKeyLen {
				return true, nil
			}
			var w [16]byte
			copy(w[:], body[:16])
			ownerBytes := body[16 : 16+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && ownerBytes[n-1] == 0 {
				n--
			}
			best.weight = w
			best.owner = string(ownerBytes[:n])
			return true, nil
		})
	if best.owner == "" {
		if envGhivediff != "" {
			fmt.Fprintf(os.Stderr, "GHIVEDIFF LP-PAY block=%d t=%d reward=%d winner=NONE\n", blockNum, blockTime, reward)
		}
		return
	}

	// Skip when the leader's weight is 0 (no positive volumes).
	allZero := true
	for _, by := range best.weight {
		if by != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		if envGhivediff != "" {
			fmt.Fprintf(os.Stderr, "GHIVEDIFF LP-PAY block=%d t=%d reward=%d winner=ZERO\n", blockNum, blockTime, reward)
		}
		return
	}
	if envGhivediff != "" {
		fmt.Fprintf(os.Stderr, "GHIVEDIFF LP-PAY block=%d t=%d reward=%d winner=%s weight=%x\n", blockNum, blockTime, reward, best.owner, best.weight)
	}

	row, err := objects.GetLiquidityRewardBalance(s, best.owner)
	if err != nil {
		return
	}
	a, aerr := objects.GetAccount(s, best.owner)
	if aerr != nil {
		return
	}
	a.Balance += reward
	objects.PutAccount(s, &a)

	// hived's pay_liquidity_reward calls adjust_supply(reward, true):
	// supply += 10×reward; fund_hive += 9×reward (post-201600).
	const startVestingBlock uint32 = 7 * 28800
	newVesting := int64(0)
	if blockNum >= startVestingBlock && reward > 0 {
		newVesting = reward * 9
	}
	dgpo.CurrentSupply += reward + newVesting
	dgpo.VirtualSupply += reward + newVesting
	dgpo.TotalVestingFundHive += newVesting
	objects.PutDGPO(s, &dgpo)

	prevWeight := row.Weight
	row.HiveVolume = 0
	row.HBDVolume = 0
	row.Weight = [16]byte{}
	row.LastUpdate = blockTime
	objects.PutLiquidityRewardBalanceUpdating(s, &row, prevWeight)
}

// liquidityRewardForVirtualSupply returns the per-hour LP reward in
// HIVE-satoshi: max(virtual_supply × 7.5% / blocks_per_year,
// HIVE_MIN_LIQUIDITY_REWARD). With block_interval=3s,
// blocks_per_year = 10,512,000.
//
// Pre-HF12 the LP reward floor is huge (1200 HIVE/hour) so until
// virtual_supply is enormous the floor dominates.
func liquidityRewardForVirtualSupply(virtualSupply int64) int64 {
	if virtualSupply <= 0 {
		return liquidityRewardMin
	}
	const aprPct = int64(750) // HIVE_LIQUIDITY_APR_PERCENT
	pct := percentRewardPerHour(virtualSupply, aprPct)
	if pct < liquidityRewardMin {
		return liquidityRewardMin
	}
	return pct
}
