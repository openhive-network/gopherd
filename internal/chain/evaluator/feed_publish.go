package evaluator

import (
	"fmt"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
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
	// hive_evaluator.cpp:597-605 — from HF20 (HIVE_HARDFORK_0_20__409) a feed
	// must be base HBD / quote HIVE. Pre-HF20 reversed feeds exist on chain
	// (f87cc43e is hived's own example), so the rule is HF-gated. The median
	// does NOT normalise orientation (hived's comment explains why it cannot:
	// it would change the price sort and therefore the median), so a reversed
	// feed accepted here would enter the median hived computes without it.
	if ctx.Hardforks.Has(hardfork.HF20) &&
		(fp.ExchangeRate.Base.Symbol.Num != asset.HbdSymbol || fp.ExchangeRate.Quote.Symbol.Num != asset.HiveSymbol) {
		return fmt.Errorf("feed_publish: price feed must be a HBD/HIVE price")
	}
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
