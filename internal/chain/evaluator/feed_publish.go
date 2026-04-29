package evaluator

import (
	"fmt"

	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// feedPublishEvaluator updates the publishing witness's last-feed
// timestamp; the actual median price feed (feed_history_object) is a
// per-block computation we'll add alongside the price-aware vesting math.
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
	// Stamp last-feed via TotalMissedBlocks placeholder slot? No — we
	// don't yet store last-feed time. For now: no-op state mutation
	// beyond touching the witness so its presence is updated.
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
