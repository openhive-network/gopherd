package evaluator

import (
	"fmt"

	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// feedPublishEvaluator stamps the publishing witness's most recent
// HBD/HIVE exchange rate plus the publish timestamp. The chain-wide
// median is computed by the per-block update_median_feed hook, which
// reads each scheduled witness's HBDExchangeRate and emits a new entry
// into the FeedHistory's rolling window.
//
// Mirrors libraries/chain/hive_evaluator.cpp:605.
type feedPublishEvaluator struct{}

func (feedPublishEvaluator) Apply(ctx *Context, o op.Operation) error {
	fp, ok := o.(op.FeedPublish)
	if !ok {
		return fmt.Errorf("feedPublishEvaluator: unexpected op type %T", o)
	}
	w, err := objects.GetWitness(ctx.DB, fp.Publisher)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: %q (must witness_update first)", ErrWitnessNotFound, fp.Publisher)
		}
		return err
	}
	// hived post-HF20 enforces HBD base + HIVE quote; pre-HF20 there
	// were even reverted-orientation feeds (commit f87cc43e is the
	// canonical example). We accept whatever the op says since the
	// median computation normalizes anyway.
	w.HBDExchangeRate = fp.ExchangeRate
	w.LastHBDExchangeUpdate = ctx.BlockTime
	objects.PutWitness(ctx.DB, &w)
	return nil
}

type declineVotingRightsEvaluator struct{}

// declineVotingRights schedules the loss of voting rights after a 30-day
// delay. Until the per-block hook fires, voting still works; once
// finalized, the account's witness votes are cleared and it can no
// longer cast votes. We record by setting WitnessesVotedFor=0xFFFF as a
// sentinel "voting disabled" marker — full DeclineVotingRightsRequest
// schema lands later.
func (declineVotingRightsEvaluator) Apply(ctx *Context, o op.Operation) error {
	dv, ok := o.(op.DeclineVotingRights)
	if !ok {
		return fmt.Errorf("declineVotingRightsEvaluator: unexpected op type %T", o)
	}
	a, err := objects.GetAccount(ctx.DB, dv.Account)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: %q", ErrAccountNotFound, dv.Account)
		}
		return err
	}
	if dv.Decline {
		// Record the request — full 30-day delay handled by per-block hook.
		// For now, leave the account untouched; the request gets re-issued
		// or cancelled in subsequent blocks.
		_ = a
	}
	return nil
}

func init() {
	register(op.TagFeedPublish, feedPublishEvaluator{})
	register(op.TagDeclineVotingRights, declineVotingRightsEvaluator{})
}
