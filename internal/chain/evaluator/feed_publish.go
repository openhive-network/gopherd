package evaluator

import (
	"fmt"

	"hived-go/internal/protocol/hardfork"
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

func (feedPublishEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagFeedPublish {
		return fmt.Errorf("feedPublishEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	fp := opArg.AsFeedPublish()
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
	w.LastHBDExchangeUpdate = ctx.HeadBlockTime
	objects.PutWitness(ctx.DB, &w)
	return nil
}

type declineVotingRightsEvaluator struct{}

// HIVE_OWNER_AUTH_RECOVERY_PERIOD = 30 days. Same window as recovery
// account changes. Mirrors libraries/protocol/include/hive/protocol/config.hpp.
const ownerAuthRecoveryPeriodSec uint32 = 60 * 60 * 24 * 30

// declineVotingRights creates / cancels a request to revoke voting
// rights after the 30-day delay. Mirrors hived's
// decline_voting_rights_evaluator (libraries/chain/hive_evaluator.cpp).
//   - decline=true: create request with effective_date = now + 30d
//   - decline=false: cancel pending request (if any)
//
// process_decline_voting_rights fires once the effective date passes.
func (declineVotingRightsEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagDeclineVotingRights {
		return fmt.Errorf("declineVotingRightsEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	dv := opArg.AsDeclineVotingRights()
	// HF14__324 — decline_voting_rights is HF14+ only.
	if !ctx.Hardforks.Has(hardfork.HF14) {
		return fmt.Errorf("decline_voting_rights: requires HF14")
	}
	if has, err := objects.HasAccount(ctx.DB, dv.Account); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: %q", ErrAccountNotFound, dv.Account)
	}
	// hived: HIVE_CHAIN_VOTING_ASSERT( account.can_vote ... ) — once an
	// account has had its voting rights effectively revoked, no further
	// decline_voting_rights ops (decline or cancel) are accepted.
	a, err := objects.GetAccount(ctx.DB, dv.Account)
	if err != nil {
		return err
	}
	if !a.CanVote() {
		return fmt.Errorf("decline_voting_rights: account %q already declined voting rights", dv.Account)
	}
	if dv.Decline {
		if has, err := objects.HasDeclineVotingRightsRequest(ctx.DB, dv.Account); err != nil {
			return err
		} else if has {
			return fmt.Errorf("decline_voting_rights: request already pending for %q", dv.Account)
		}
		objects.PutDeclineVotingRightsRequest(ctx.DB, &objects.DeclineVotingRightsRequest{
			Account:       dv.Account,
			EffectiveDate: ctx.HeadBlockTime + ownerAuthRecoveryPeriodSec,
		})
		return nil
	}
	// Cancel
	req, err := objects.GetDeclineVotingRightsRequest(ctx.DB, dv.Account)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("decline_voting_rights: no pending request for %q", dv.Account)
		}
		return err
	}
	objects.DeleteDeclineVotingRightsRequestFull(ctx.DB, &req)
	return nil
}

func init() {
	register(op.TagFeedPublish, feedPublishEvaluator{})
	register(op.TagDeclineVotingRights, declineVotingRightsEvaluator{})
}
