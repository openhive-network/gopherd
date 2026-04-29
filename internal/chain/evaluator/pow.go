package evaluator

import (
	"fmt"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

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
	pay := minPowReward
	if ctx.BlockNum < startMinerVotingBlockPow {
		pay *= maxWitnesses
	}
	if _, err := objects.GetAccount(ctx.DB, witnessName); err != nil {
		return nil
	}
	// hived: adjust_supply then either credit balance directly (pre-864000)
	// or call create_vesting (post-864000). Both branches grow current_supply.
	dgpo.CurrentSupply += pay
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
			Name:    worker,
			Created: ctx.BlockTime,
		})
	}
	// Register the worker as a witness if missing — hived's pow_apply
	// calls db.create<witness_object>(...) on first pow from this
	// account, alongside the account creation.
	wExists, err := objects.HasWitness(ctx.DB, worker)
	if err != nil {
		return err
	}
	if !wExists {
		objects.PutWitness(ctx.DB, &objects.Witness{
			Owner:   worker,
			Created: ctx.BlockTime,
		})
	}
	if !payReward {
		return nil
	}
	return payPowReward(ctx)
}

func init() {
	register(op.TagPow, powEvaluator{})
	register(op.TagPow2, pow2Evaluator{})
}
