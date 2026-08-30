package evaluator

import (
	"fmt"

	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// HIVE_MAX_RECURRENT_TRANSFER_END_DATE — duration cap on the schedule
// (24 months in seconds) per hive_evaluator_transfer.cpp:970.
const recurrentTransferMaxEndSec uint32 = 730 * 24 * 60 * 60

type recurrentTransferEvaluator struct{}

// extractRecurrentTransferPairID parses the pair_id extension from a
// recurrent_transfer op. The extension wire format is a flat_set of
// static_variant<void_t, recurrent_transfer_pair_id>:
//
//	varint count
//	for each: varint tag (0 = void, 1 = pair_id), uint8 body if tag=1
//
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
	if opArg.Tag != op.TagRecurrentTransfer {
		return fmt.Errorf("recurrentTransferEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	rt := opArg.AsRecurrentTransfer()
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

	from, err := objects.GetAccount(ctx.DB, rt.From)
	if err != nil {
		return fmt.Errorf("%w: from=%q", ErrAccountNotFound, rt.From)
	}
	if has, err := objects.HasAccount(ctx.DB, rt.To); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: to=%q", ErrAccountNotFound, rt.To)
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

	// Branch layout mirrors C++ recurrent_transfer_evaluator::do_apply
	// (hive_evaluator_transfer.cpp:952+): the (from,to,pair_id) lookup
	// decides create vs delete vs modify. amount == 0 on an EXISTING
	// schedule is the documented CANCEL path (validate() already enforces
	// executions >= 1, so a mainnet cancel carries executions >= 2 — the
	// old `amount==0 && executions==0` delete gate here matched nothing
	// and cancels bounced with "amount must be non-zero", first at block
	// 55,240,835, HF25+5,068).
	existing, existingErr := objects.GetRecurrentTransfer(ctx.DB, rt.From, rt.To, pairID)
	hasExisting := existingErr == nil

	if hasExisting && rt.Amount.Amount == 0 {
		// Delete. Pre-HF29 nodes reject executions < 2 on this path too
		// (hive_evaluator_transfer.cpp:989-997).
		//
		// NOTE the gate: this rule and the modify one below are checked on
		// the HARDFORK ALONE, with no ctx.InControl half, and that asymmetry
		// against the other three HF29 rules is deliberate on hived's part.
		// The others TIGHTEN validation, so applying them early at the head
		// only refuses transactions the network is about to refuse anyway.
		// This one RELAXES it. Accepting executions == 1 before HF29 actually
		// activates would mean admitting and relaying transactions every
		// other node still rejects — we would build blocks the network orphans.
		// A loosening rule must wait for the fork; a tightening rule need not.
		if !ctx.Hardforks.Has(hardfork.HF29) && rt.Executions < 2 {
			return fmt.Errorf("recurrent_transfer: executions must be at least 2 when deleting (pre-HF29)")
		}
		objects.DeleteRecurrentTransfer(ctx.DB, rt.From, rt.To, pairID)
		// C++ asserts open_recurrent_transfers > 0 before decrement;
		// saturated-floor defensively.
		if from.OpenRecurrentTransfers > 0 {
			from.OpenRecurrentTransfers--
			objects.PutAccount(ctx.DB, &from)
		}
		return nil
	}
	if !hasExisting {
		// Create-path asserts, in C++ order.
		if from.OpenRecurrentTransfers >= 255 {
			return fmt.Errorf("recurrent_transfer: %q already has 255 open recurrent transfers (HIVE_MAX_OPEN_RECURRENT_TRANSFERS)", rt.From)
		}
		if rt.Amount.Amount == 0 {
			return fmt.Errorf("recurrent_transfer: cannot create a recurrent transfer with 0 amount (no existing schedule to delete)")
		}
		if rt.Executions < 2 {
			return fmt.Errorf("recurrent_transfer: executions must be at least 2 on create")
		}
	} else if !ctx.Hardforks.Has(hardfork.HF29) && rt.Executions < 2 {
		// Modify path (hive_evaluator_transfer.cpp:1005-1017). HF29
		// ALLOW_MODIFY_LAST_RECURRENT_TRANSFER (issue #786) lets executions
		// drop to 1 so the final payment of an existing schedule can still
		// have its amount or memo changed. Before HF29 that was refused
		// because a fresh 1-execution transfer fires immediately and deletes
		// itself; on an EXISTING schedule that reasoning does not hold, which
		// is why only the create path keeps the >= 2 floor after HF29. Gated
		// on the hardfork alone — see the delete path above for why.
		return fmt.Errorf("recurrent_transfer: executions must be at least 2 when modifying (pre-HF29)")
	}

	// Trigger-date semantics (C++ set_recurrence_trigger_date): resets only
	// when recurrence changes; a fresh create triggers at head_block_time so
	// the first firing happens in the containing block
	// (hive_evaluator_transfer.cpp:969 + recurrent_transfer_object.hpp:18).
	var newTrigger uint32
	switch {
	case !hasExisting:
		newTrigger = ctx.HeadBlockTime
	case existing.Recurrence == rt.Recurrence:
		newTrigger = existing.Trigger
	default:
		newTrigger = ctx.HeadBlockTime + uint32(rt.Recurrence)*3600
	}

	// Duration cap: get_final_trigger_date() <= head + HIVE_MAX_RECURRENT_TRANSFER_END_DATE.
	// get_final_trigger_date = trigger_date + recurrence_hours * (executions - 1).
	// See recurrent_transfer_object.hpp::get_final_trigger_date and
	// hive_evaluator_transfer.cpp:970.
	if rt.Executions > 0 {
		final := uint64(newTrigger) + uint64(rt.Recurrence)*uint64(rt.Executions-1)*3600
		cap := uint64(ctx.HeadBlockTime) + uint64(recurrentTransferMaxEndSec)
		if final > cap {
			return fmt.Errorf("recurrent_transfer: schedule duration exceeds 24-month cap (too long)")
		}
	}

	var consecFailures uint8
	if hasExisting {
		consecFailures = existing.ConsecutiveFailures
	} else {
		// CREATE: increment open_recurrent_transfers (the <255 assert ran
		// above, in C++ order). Updates don't touch the counter.
		from.OpenRecurrentTransfers++
		objects.PutAccount(ctx.DB, &from)
	}

	objects.PutRecurrentTransfer(ctx.DB, &objects.RecurrentTransfer{
		From:                rt.From,
		To:                  rt.To,
		PairID:              pairID,
		Amount:              rt.Amount.Amount,
		Symbol:              uint32(rt.Amount.Symbol.Num),
		Memo:                rt.Memo,
		Recurrence:          rt.Recurrence,
		Executions:          rt.Executions,
		Trigger:             newTrigger,
		ConsecutiveFailures: consecFailures,
	})
	_ = state.ErrNotFound // keep import
	return nil
}

func init() {
	register(op.TagRecurrentTransfer, recurrentTransferEvaluator{})
}
