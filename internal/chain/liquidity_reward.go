package chain

import (
	"fmt"
	"os"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
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
//  1. Compute reward = max(virtual_supply × HIVE_LIQUIDITY_APR_PERCENT
//     / 100% / HIVE_BLOCKS_PER_HOUR, HIVE_MIN_LIQUIDITY_REWARD).
//  2. Find the highest-weight liquidity_reward_balance row.
//  3. If its weight > 0:
//     adjust_supply(+reward) — mints new HIVE.
//     adjust_balance(owner, +reward) — credits the LP.
//     Reset row's volumes/weight to 0; refresh last_update.
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
	// index. The on-disk index is keyed [weight:16][owner_padded:16] in
	// ascending byte order, so the LARGEST weight comes last; we collect
	// candidates and break weight-ties in C++ order. C++'s by_volume_weight
	// uses composite_key_compare<std::greater<weight>, std::less<account_id_type>>,
	// so on a weight-tie the OLDER account (smaller account_id) wins.
	// Go has no account_id but Account.Created (block-time) is allocated
	// in the same chronological order across accounts created in
	// different blocks, so we tie-break on Created first then Name lex.
	// Without this, Go's "last entry wins" tie-break picks the lex-largest
	// owner — at block 3,259,200 that flipped LP-PAY winner from `afew`
	// (C++) to `alot` (Go) when both held weight 0x16e360, cascading into
	// alittle's 900-HIVE shortfall at block 3,342,630.
	type candidate struct {
		weight  [16]byte
		owner   string
		created uint32
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
			owner := string(ownerBytes[:n])
			cmp := bytesCompare16(w, best.weight)
			if best.owner == "" || cmp > 0 {
				a, _ := objects.GetAccount(s, owner)
				best = candidate{weight: w, owner: owner, created: a.Created}
				return true, nil
			}
			if cmp == 0 {
				a, _ := objects.GetAccount(s, owner)
				if a.Created < best.created || (a.Created == best.created && owner < best.owner) {
					best = candidate{weight: w, owner: owner, created: a.Created}
				}
			}
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

// bytesCompare16 returns -1/0/+1 for big-endian byte-wise compare.
func bytesCompare16(a, b [16]byte) int {
	for i := 0; i < 16; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
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
