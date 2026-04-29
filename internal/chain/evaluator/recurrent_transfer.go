package evaluator

import (
	"fmt"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

type recurrentTransferEvaluator struct{}

// recurrent_transfer creates / updates / deletes a recurrence schedule.
// Hived's per-block hook fires the actual periodic transfers; we record
// the schedule and let the hook (when wired up) execute it.
//
// Op semantics:
//   - amount = 0 AND executions = 0 → delete schedule
//   - otherwise update or create schedule
func (recurrentTransferEvaluator) Apply(ctx *Context, o op.Operation) error {
	rt, ok := o.(op.RecurrentTransfer)
	if !ok {
		return fmt.Errorf("recurrentTransferEvaluator: unexpected op type %T", o)
	}
	if rt.Amount.Symbol.Num != asset.HiveSymbol && rt.Amount.Symbol.Num != asset.HbdSymbol {
		return fmt.Errorf("%w: recurrent_transfer", ErrUnsupportedSymbol)
	}
	if rt.Amount.Amount < 0 {
		return ErrTransferAmountNotPos
	}
	// pair_id is in the extensions; for now use 0.
	pairID := uint8(0)

	if rt.Amount.Amount == 0 && rt.Executions == 0 {
		// Delete the schedule.
		if has, err := objects.HasRecurrentTransfer(ctx.DB, rt.From, rt.To, pairID); err != nil {
			return err
		} else if has {
			objects.DeleteRecurrentTransfer(ctx.DB, rt.From, rt.To, pairID)
		}
		return nil
	}

	if has, err := objects.HasAccount(ctx.DB, rt.From); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: from=%q", ErrAccountNotFound, rt.From)
	}
	if has, err := objects.HasAccount(ctx.DB, rt.To); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: to=%q", ErrAccountNotFound, rt.To)
	}
	objects.PutRecurrentTransfer(ctx.DB, &objects.RecurrentTransfer{
		From:       rt.From,
		To:         rt.To,
		PairID:     pairID,
		Amount:     rt.Amount.Amount,
		Symbol:     uint32(rt.Amount.Symbol.Num),
		Memo:       rt.Memo,
		Recurrence: rt.Recurrence,
		Executions: rt.Executions,
		Trigger:    ctx.BlockTime + uint32(rt.Recurrence)*3600,
	})
	_ = state.ErrNotFound // keep import
	return nil
}

func init() {
	register(op.TagRecurrentTransfer, recurrentTransferEvaluator{})
}
