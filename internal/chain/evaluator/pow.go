package evaluator

import (
	"fmt"
	"math/big"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_POW_APR_PERCENT — POW APR target (libraries/protocol/include/
// hive/protocol/config.hpp). Used by C++'s
// calc_percent_reward_per_round< HIVE_POW_APR_PERCENT >(virtual_supply).
const powAPRPercent int64 = 750

// percentRewardPerRound constants mirror hived's compound.hpp:38 with
// HIVE_APR_PERCENT_*_PER_ROUND. Computed once at build time.
const (
	powAprMultiplyPerRound uint64 = 0x79cc_f5c7_3480
	powAprShiftPerRound    uint   = 83
)

// powPercentRewardPerRound mirrors C++'s
// calc_percent_reward_per_round<percent>(virtual_supply) — supply ×
// percent × multiply / 2^shift, rounded half-up. Used by get_pow_reward
// to compute the percent floor before max() with HIVE_MIN_POW_REWARD.
func powPercentRewardPerRound(virtualSupply, aprPercent int64) int64 {
	if virtualSupply <= 0 || aprPercent <= 0 {
		return 0
	}
	prod := new(big.Int).Mul(big.NewInt(virtualSupply), big.NewInt(aprPercent))
	prod.Mul(prod, new(big.Int).SetUint64(powAprMultiplyPerRound))
	half := new(big.Int).Lsh(big.NewInt(1), powAprShiftPerRound-1)
	prod.Add(prod, half)
	prod.Rsh(prod, powAprShiftPerRound)
	if !prod.IsInt64() {
		return int64(^uint64(0) >> 1)
	}
	return prod.Int64()
}

// powEvaluator implements the partial-mining-era apply for `pow`.
//
// Full hived semantics: validate PoW (skipped during replay), create
// the worker account if absent, create/refresh a witness_object, pay
// HIVE_MINING_REWARD as VESTS, and update DGPO totals.
//
// What this implementation handles:
//   - Account creation when missing (so subsequent transfers find the
//     account). Balances start at zero; the mining reward is deferred
//     until vesting accounting is real.
//
// What's intentionally elided (added with the relevant state objects):
//   - witness_object create/update.
//   - reward payout / supply adjustment.
//   - PoW hash validation.
type powEvaluator struct{}

func (powEvaluator) Apply(ctx *Context, o op.Operation) error {
	pw, ok := o.(op.Pow)
	if !ok {
		return fmt.Errorf("powEvaluator: unexpected op type %T", o)
	}
	// pow1 always pays the reward (it's deprecated at HF13 and chains
	// past that point reject the op entirely; the block_log won't
	// contain pow1 ops post-HF13).
	return ensureMinerAccountForPow(ctx, pw.WorkerAccount, true)
}

// pow2Evaluator implements the partial-mining-era apply for `pow2`. Same
// scope as powEvaluator: create the worker account if absent.
type pow2Evaluator struct{}

func (pow2Evaluator) Apply(ctx *Context, o op.Operation) error {
	pw, ok := o.(op.Pow2)
	if !ok {
		return fmt.Errorf("pow2Evaluator: unexpected op type %T", o)
	}
	var worker string
	switch pw.WorkVariantTag {
	case 0: // pow2 variant
		worker = pw.PowInput.WorkerAccount
	case 1: // equihash_pow variant
		worker = pw.EquihashWork.Input.WorkerAccount
	default:
		return fmt.Errorf("pow2: unknown work variant %d", pw.WorkVariantTag)
	}
	// hived's pow2_evaluator (libraries/chain/hive_evaluator.cpp:593) only
	// pays the reward when HF16 is NOT yet active. Account/witness setup
	// always runs.
	return ensureMinerAccountForPow(ctx, worker, !ctx.Hardforks.Has(hardfork.HF16))
}

// HIVE_MIN_POW_REWARD = HIVE_MINING_REWARD = 1.000 HIVE in satoshi
// (libraries/protocol/include/hive/protocol/config.hpp).
const minPowReward int64 = 1000

// HIVE_MAX_WITNESSES — used to multiply the pow reward pre-HIVE_START_MINER_VOTING_BLOCK.
const maxWitnesses int64 = 21

// HIVE_START_MINER_VOTING_BLOCK = 30 days × 28800 = 864,000.
const startMinerVotingBlockPow uint32 = 864_000

// payPowReward implements the C++ pow_apply reward distribution:
//
//	pow_reward = HIVE_MIN_POW_REWARD                                      // formula floor
//	if (head_block < HIVE_START_MINER_VOTING_BLOCK)
//	    pow_reward *= HIVE_MAX_WITNESSES                                  // 21x boost early
//	dgpo.current_supply += pow_reward
//	if (head_block < HIVE_START_MINER_VOTING_BLOCK)
//	    inc_witness.balance += pow_reward                                 // liquid HIVE
//	else
//	    inc_witness.vesting_shares += create_vesting(pow_reward)          // VESTS
//
// The reward goes to the BLOCK PRODUCER (dgp.current_witness), not the
// worker — a critical detail. The worker's only state effect is having
// its account created if it didn't exist yet.
func payPowReward(ctx *Context) error {
	dgpo, err := objects.GetDGPO(ctx.DB)
	if err != nil && err != state.ErrNotFound {
		return err
	}
	witnessName := ctx.Witness
	if witnessName == "" {
		return nil
	}
	// hived bumps num_pow_witnesses on EVERY pow op (pow_apply at
	// hive_evaluator.cpp:454) — before computing get_pow_reward — so the
	// 21st pow op overall is the first to pay non-zero. Pre-HIVE_START_
	// VESTING_BLOCK (201,600), get_pow_reward returns 0 until num >= 21
	// (database.cpp:1940). Without this gate, pre-201,600 pow ops pay
	// 21,000 satoshi each from block 1, inflating cur_supply by ~420K
	// vs hived (visible at the HF7 cashout cascade as a constant 1.6%
	// drift).
	dgpo.NumPowWitnesses++
	const startVestingBlock uint32 = 7 * 28800 // HIVE_START_VESTING_BLOCK
	if ctx.BlockNum < startVestingBlock && dgpo.NumPowWitnesses < uint32(maxWitnesses) {
		objects.PutDGPO(ctx.DB, &dgpo)
		return nil
	}
	// Mirror C++'s get_pow_reward (database.cpp:1933): take the max of
	// HIVE_MIN_POW_REWARD and calc_percent_reward_per_round< 7.5% >
	// (virtual_supply). Once virtual_supply gets large (post ~5M blocks),
	// the percent term overtakes the floor and we'd otherwise underpay
	// every POW miner by a few satoshi each — quickly diverging supply.
	virtualSupply := dgpo.VirtualSupply
	if virtualSupply <= 0 {
		virtualSupply = dgpo.CurrentSupply
	}
	pay := powPercentRewardPerRound(virtualSupply, powAPRPercent)
	if pay < minPowReward {
		pay = minPowReward
	}
	if ctx.BlockNum < startMinerVotingBlockPow {
		pay *= maxWitnesses
	}
	if _, err := objects.GetAccount(ctx.DB, witnessName); err != nil {
		objects.PutDGPO(ctx.DB, &dgpo)
		return nil
	}
	// hived's adjust_supply(pow_reward, /*adjust_vesting=*/true) at
	// database.cpp:3357. When head ≥ HIVE_START_VESTING_BLOCK
	// (7×28800 = 201600) and delta > 0, fund_hive grows by 9× the
	// reward and supply by 10×. Pre-201600 the vesting leg is
	// suppressed (only current_supply += pay).
	newVesting := int64(0)
	if ctx.BlockNum >= startVestingBlock && pay > 0 {
		newVesting = pay * 9
	}
	dgpo.CurrentSupply += pay + newVesting
	dgpo.VirtualSupply += pay + newVesting
	dgpo.TotalVestingFundHive += newVesting
	objects.PutDGPO(ctx.DB, &dgpo)

	if ctx.BlockNum < startMinerVotingBlockPow {
		w, err := objects.GetAccount(ctx.DB, witnessName)
		if err != nil {
			return nil
		}
		w.Balance += pay
		objects.PutAccount(ctx.DB, &w)
	} else {
		if _, err := CreateVesting(ctx.DB, witnessName, pay, false); err != nil {
			return err
		}
	}
	return nil
}

// ensureMinerAccountForPow creates an account named worker if missing
// and (when payReward is true) credits the block producer with the
// pow reward.
func ensureMinerAccountForPow(ctx *Context, worker string, payReward bool) error {
	if worker == "" {
		return fmt.Errorf("pow/pow2: empty worker_account")
	}
	has, err := objects.HasAccount(ctx.DB, worker)
	if err != nil {
		return err
	}
	if !has {
		objects.PutAccount(ctx.DB, &objects.Account{
			Name:                 worker,
			Created:              ctx.HeadBlockTime,
			VotingManaCurrent:    10000,
			VotingManaLastUpdate: ctx.HeadBlockTime,
		})
	}
	// Register the worker as a witness if missing — hived's pow_apply
	// calls db.create<witness_object>(...) on first pow from this
	// account, alongside the account creation. Mark them in the
	// PoW miner queue (witness_object::pow_worker = head_block_num+1
	// so it's monotonic and never zero) and bump
	// dgpo.num_pow_witnesses; update_witness_schedule's miner-slot
	// pass will pop them later. Existing witnesses re-running PoW
	// also bump the queue position.
	wExists, err := objects.HasWitness(ctx.DB, worker)
	if err != nil {
		return err
	}
	var w objects.Witness
	if !wExists {
		w = objects.Witness{
			Owner:   worker,
			Created: ctx.HeadBlockTime,
		}
		seedVirtualScheduledTime(&w)
	} else {
		got, err := objects.GetWitness(ctx.DB, worker)
		if err != nil {
			return err
		}
		w = got
	}
	w.PowWorker = uint64(ctx.BlockNum) + 1
	objects.PutWitness(ctx.DB, &w)
	if !payReward {
		return nil
	}
	return payPowReward(ctx)
}

func init() {
	register(op.TagPow, powEvaluator{})
	register(op.TagPow2, pow2Evaluator{})
}
