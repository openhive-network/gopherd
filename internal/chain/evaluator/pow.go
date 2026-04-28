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

// hiveMiningReward is HIVE_MINING_REWARD (libraries/protocol/include/hive/protocol/config.hpp):
// `asset(1000, HIVE_SYMBOL)` — 1.000 HIVE per pow op. hived's mining flow
// converts this to VESTS via create_vesting; we credit the liquid Balance
// to keep subsequent transfers funded. CurrentSupply on the DGPO is
// adjusted to track the inflation.
const hiveMiningReward int64 = 1000

// ensureMinerAccount creates an account named worker if missing AND
// credits the standard mining reward to the worker's HIVE balance.
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
	a, err := objects.GetAccount(ctx.DB, worker)
	if err != nil {
		return err
	}
	a.Balance += hiveMiningReward
	objects.PutAccount(ctx.DB, &a)

	dgpo, err := objects.GetDGPO(ctx.DB)
	if err != nil && err != state.ErrNotFound {
		return err
	}
	dgpo.CurrentSupply += hiveMiningReward
	objects.PutDGPO(ctx.DB, &dgpo)
	return nil
}

func init() {
	register(op.TagPow, powEvaluator{})
	register(op.TagPow2, pow2Evaluator{})
}
