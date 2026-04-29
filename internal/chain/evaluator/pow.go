package evaluator

import (
	"fmt"

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
	return ensureMinerAccount(ctx, pw.WorkerAccount)
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
	return ensureMinerAccount(ctx, worker)
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
	w, err := objects.GetAccount(ctx.DB, witnessName)
	if err != nil {
		return nil
	}
	if ctx.BlockNum < startMinerVotingBlockPow {
		w.Balance += pay
		dgpo.CurrentSupply += pay
	} else {
		// HIVE → VESTS at the 1:1 price scaled by precision (see
		// HiveToVestsRatio). VESTS has 6 decimals, HIVE 3.
		w.VestingShares += pay * HiveToVestsRatio
		dgpo.TotalVestingShares += pay * HiveToVestsRatio
	}
	objects.PutAccount(ctx.DB, &w)
	objects.PutDGPO(ctx.DB, &dgpo)
	return nil
}

// ensureMinerAccount creates an account named worker if missing.
// No balance is credited — the pow reward goes to the block producer
// (see payPowReward).
func ensureMinerAccount(ctx *Context, worker string) error {
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
	return payPowReward(ctx)
}

func init() {
	register(op.TagPow, powEvaluator{})
	register(op.TagPow2, pow2Evaluator{})
}
