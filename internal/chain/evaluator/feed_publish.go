package evaluator

import (
	"fmt"
	"math/bits"

	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
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
	if err := checkFeedNearMedian(ctx, fp.ExchangeRate); err != nil {
		return err
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

// maxFeedDeviationFromMedian is hived's max_deviation in
// check_feed_near_median (67a927815). A variable so tests can turn it on in a
// testnet build.
var maxFeedDeviationFromMedian = network.MaxFeedDeviationFromMedian

// checkFeedNearMedian refuses a price feed more than
// maxFeedDeviationFromMedian times above or below the market median of the
// feed history, most likely a wrong source or a typo. Node policy, as in
// hived: only for what this node takes in or produces (InControl), never for
// other producers' blocks, so block validity does not change.
func checkFeedNearMedian(ctx *Context, feed asset.Price) error {
	if maxFeedDeviationFromMedian == 0 || !ctx.InControl {
		return nil
	}
	h, err := objects.GetFeedHistory(ctx.DB)
	if err != nil {
		return nil
	}
	m := h.MarketMedianHistory
	if m.Base.Amount <= 0 || m.Quote.Amount <= 0 || feed.Base.Amount <= 0 || feed.Quote.Amount <= 0 {
		return nil // null median; non-positive feeds are refused by validation
	}
	// feed/median through cross products, which fit in 128 bits.
	fHi, fLo := bits.Mul64(uint64(feed.Base.Amount), uint64(m.Quote.Amount))
	mHi, mLo := bits.Mul64(uint64(m.Base.Amount), uint64(feed.Quote.Amount))
	if !le128(div128(fHi, fLo, maxFeedDeviationFromMedian), mHi, mLo) ||
		!le128(div128(mHi, mLo, maxFeedDeviationFromMedian), fHi, fLo) {
		return fmt.Errorf("price feed %d/%d is more than %d times above or below the market median %d/%d; "+
			"this node does not take it (a check of this node, not a rule for blocks)",
			feed.Base.Amount, feed.Quote.Amount, maxFeedDeviationFromMedian, m.Base.Amount, m.Quote.Amount)
	}
	return nil
}

// div128 is (hi:lo) / d as a 128-bit (hi, lo) pair.
func div128(hi, lo, d uint64) [2]uint64 {
	qHi := hi / d
	qLo, _ := bits.Div64(hi%d, lo, d)
	return [2]uint64{qHi, qLo}
}

// le128 reports a <= (hi:lo).
func le128(a [2]uint64, hi, lo uint64) bool {
	return a[0] < hi || (a[0] == hi && a[1] <= lo)
}
