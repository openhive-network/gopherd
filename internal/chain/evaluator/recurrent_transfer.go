package evaluator

import (
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// HIVE_MAX_RECURRENT_TRANSFER_END_DATE — duration cap on the schedule
// (24 months in seconds) per hive_evaluator_transfer.cpp:970.
const recurrentTransferMaxEndSec uint32 = 730 * 24 * 60 * 60

type recurrentTransferEvaluator struct{}

// extractRecurrentTransferPairID parses the pair_id extension from a
// recurrent_transfer op. The extension wire format is a flat_set of
// static_variant<void_t, recurrent_transfer_pair_id>:
//   varint count
//   for each: varint tag (0 = void, 1 = pair_id), uint8 body if tag=1
// Returns the pair_id (0 if no pair_id extension is present).
func extractRecurrentTransferPairID(raw []byte) uint8 {
	if len(raw) == 0 {
		return 0
	}
	r := fcraw.NewReader(raw)
	count, err := r.ReadVarUint()
	if err != nil {
		return 0
	}
	for i := uint32(0); i < count; i++ {
		tag, err := r.ReadVarUint()
		if err != nil {
			return 0
		}
		switch tag {
		case 0:
			// void_t — no body
		case 1:
			// recurrent_transfer_pair_id — uint8
			b, err := r.ReadByte()
			if err != nil {
				return 0
			}
			return b
		default:
			return 0
		}
	}
	return 0
}

// recurrent_transfer creates / updates / deletes a recurrence schedule.
// Hived's per-block hook fires the actual periodic transfers; we record
// the schedule and let the hook execute it.
//
// Op semantics:
//   - amount = 0 AND executions = 0 → delete schedule (CRUD delete)
//   - otherwise update or create schedule keyed by (from, to, pair_id)
//
// pair_id (HF26+) lets one (from, to) pair carry multiple parallel
// schedules, parsed from the extensions field.
func (recurrentTransferEvaluator) Apply(ctx *Context, opArg op.Op) error {
	o := opArg.Operation()
	rt, ok := o.(op.RecurrentTransfer)
	if !ok {
		return fmt.Errorf("recurrentTransferEvaluator: unexpected op type %T", o)
	}
	// HF1_25 — recurrent_transfer is HF25+ only.
	if !ctx.Hardforks.Has(hardfork.HF25) {
		return fmt.Errorf("recurrent_transfer: requires HF25")
	}
	if rt.Amount.Symbol.Num != asset.HiveSymbol && rt.Amount.Symbol.Num != asset.HbdSymbol {
		return fmt.Errorf("%w: recurrent_transfer", ErrUnsupportedSymbol)
	}
	if rt.Amount.Amount < 0 {
		return ErrTransferAmountNotPos
	}
	pairID := extractRecurrentTransferPairID(rt.Extensions)

	if rt.Amount.Amount == 0 && rt.Executions == 0 {
		// Delete the schedule.
		if has, err := objects.HasRecurrentTransfer(ctx.DB, rt.From, rt.To, pairID); err != nil {
			return err
		} else if has {
			objects.DeleteRecurrentTransfer(ctx.DB, rt.From, rt.To, pairID)
		}
		return nil
	}

	// hive_evaluator_transfer.cpp:968 — create path requires non-zero
	// amount.
	if rt.Amount.Amount == 0 {
		return fmt.Errorf("recurrent_transfer: amount must be non-zero")
	}

	from, err := objects.GetAccount(ctx.DB, rt.From)
	if err != nil {
		return fmt.Errorf("%w: from=%q", ErrAccountNotFound, rt.From)
	}
	if has, err := objects.HasAccount(ctx.DB, rt.To); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: to=%q", ErrAccountNotFound, rt.To)
	}

	// Duration cap: recurrence × executions ≤ HIVE_MAX_RECURRENT_TRANSFER_END_DATE.
	// recurrence is in hours, executions is the count. Hived computes
	// final_trigger_date = head + recurrence_hours × executions × 3600
	// (hive_evaluator_transfer.cpp:970).
	totalSec := uint64(rt.Recurrence) * uint64(rt.Executions) * 3600
	if totalSec > uint64(recurrentTransferMaxEndSec) {
		return fmt.Errorf("recurrent_transfer: schedule duration exceeds 24-month cap")
	}

	// First-execution balance check (hive_evaluator_transfer.cpp:957) —
	// only the first transfer has to be funded; subsequent ones can fail
	// silently.
	switch rt.Amount.Symbol.Num {
	case asset.HiveSymbol:
		if from.Balance < rt.Amount.Amount {
			return fmt.Errorf("%w: %q insufficient HIVE for first execution", ErrInsufficientBalance, rt.From)
		}
	case asset.HbdSymbol:
		if from.HBDBalance < rt.Amount.Amount {
			return fmt.Errorf("%w: %q insufficient HBD for first execution", ErrInsufficientBalance, rt.From)
		}
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
		Trigger:    ctx.HeadBlockTime + uint32(rt.Recurrence)*3600,
	})
	_ = state.ErrNotFound // keep import
	return nil
}

func init() {
	register(op.TagRecurrentTransfer, recurrentTransferEvaluator{})
}
