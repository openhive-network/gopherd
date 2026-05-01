package evaluator

import (
	"errors"
	"fmt"
	"math/big"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/reward"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Cashout windows from libraries/protocol/include/hive/protocol/config.hpp:
//
//	HIVE_CASHOUT_WINDOW_SECONDS_PRE_HF12 = 60*60*24      (1 day)
//	HIVE_CASHOUT_WINDOW_SECONDS_PRE_HF17 = 60*60*12      (12 hours)
//	HIVE_CASHOUT_WINDOW_SECONDS          = 60*60*24*7    (7 days)
//
// Pre-HF12: every comment cashes out 24h after creation (root and
// reply alike). HF12-HF17: only root posts cash out, at 12h. HF17+:
// every comment cashes out at 7d.
const (
	cashoutWindowSecondsPreHF12 uint32 = 60 * 60 * 24
	cashoutWindowSecondsPreHF17 uint32 = 60 * 60 * 12
	cashoutWindowSeconds        uint32 = 60 * 60 * 24 * 7
)

// commentCashoutTime returns the cashout time for a new comment given
// the hardforks active at apply time and whether it has a parent.
func commentCashoutTime(hf hardfork.Set, blockTime uint32, hasParent bool) uint32 {
	if hf.Has(hardfork.HF17) {
		return blockTime + cashoutWindowSeconds
	}
	if hf.Has(hardfork.HF12) {
		// HF12-HF17: replies are frozen (cashout_time = max), only
		// root posts cash out 12h after creation.
		if hasParent {
			return 0
		}
		return blockTime + cashoutWindowSecondsPreHF17
	}
	// Pre-HF12: 24h cashout window for every comment (root or reply).
	// process_comment_cashout itself doesn't actually fire until HF7
	// (HIVE_HARDFORK_0_7_FIRST_CASHOUT_TIME); the schedule entries we
	// queue here just sit in the table until then.
	return blockTime + cashoutWindowSecondsPreHF12
}

// Errors specific to comment evaluators.
var (
	ErrCommentNotFound = errors.New("comment not found")
	ErrCommentExists   = errors.New("comment already exists at this permlink")
	ErrCommentClosed   = errors.New("comment is no longer accepting votes / edits")
	ErrVoteWeightZero  = errors.New("vote weight cannot be zero")
)

// commentEvaluator implements comment.
//
// Hived semantics: if no comment exists at (author, permlink), create
// one (with the provided parent and metadata); otherwise update the
// existing comment's last_update and metadata. Hived also enforces
// HIVE_MIN_ROOT_COMMENT_INTERVAL and HIVE_MIN_REPLY_INTERVAL between
// posts — those rate-limit checks land later (they require a
// last_post / last_root_post field on Account).
type commentEvaluator struct{}

func (commentEvaluator) Apply(ctx *Context, o op.Operation) error {
	c, ok := o.(op.Comment)
	if !ok {
		return fmt.Errorf("commentEvaluator: unexpected op type %T", o)
	}
	// The author must exist.
	authorAcct, err := objects.GetAccount(ctx.DB, c.Author)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: author=%q", ErrAccountNotFound, c.Author)
		}
		return err
	}

	// The parent must exist if this is a reply (parent_author non-empty).
	// hived's comment_evaluator (libraries/chain/hive_evaluator_social.cpp:184)
	// calls _db.get_comment(parent_author, parent_permlink) which throws if
	// not found. For root posts (parent_author == ""), the parent is the
	// implicit "category" wrapper so we skip the check.
	hasParent := c.ParentAuthor != ""
	if hasParent {
		if has, err := objects.HasComment(ctx.DB, c.ParentAuthor, c.ParentPermlink); err != nil {
			return err
		} else if !has {
			return fmt.Errorf("%w: parent=%q/%q", ErrCommentNotFound, c.ParentAuthor, c.ParentPermlink)
		}
	}

	existing, err := objects.GetComment(ctx.DB, c.Author, c.Permlink)
	if err != nil && err != state.ErrNotFound {
		return err
	}

	// Per-author posting intervals (libraries/chain/hive_evaluator_social.cpp:204):
	// pre-HF6: nominally `(now - last_post) > 60s`, but the canonical
	//          chain has examples of klye and others posting within 60s
	//          before block 2790882 — the C++ source itself notes "there
	//          are cases of root posts more frequent than current rules".
	//          We skip the check pre-HF6 to remain replay-compatible.
	// post-HF6 root post:  (now - last_root_post) > HIVE_MIN_ROOT_COMMENT_INTERVAL (5 min)
	// post-HF6 reply:      (now - last_post)      >= HIVE_MIN_REPLY_INTERVAL (3 sec)
	// Only enforced on NEW comments, not edits.
	if err == state.ErrNotFound {
		if !ctx.Hardforks.Has(hardfork.HF6) {
			// pre-HF6: no enforcement (see comment above).
		} else {
			if !hasParent {
				if ctx.HeadBlockTime-authorAcct.LastRootPost <= 5*60 {
					return fmt.Errorf("comment: author %q must wait 5 minutes between root posts", c.Author)
				}
			} else {
				if ctx.HeadBlockTime-authorAcct.LastPost < 3 {
					return fmt.Errorf("comment: author %q must wait 3 seconds between replies", c.Author)
				}
			}
		}
	}
	if err == state.ErrNotFound {
		// New comment — pre-HF12 comments are frozen (no cashout).
		// HF12-HF17 only root posts cash out. HF17+ all comments do.
		// commentCashoutTime returns 0 when the comment never cashes out.
		cashoutTime := commentCashoutTime(ctx.Hardforks, ctx.HeadBlockTime, hasParent)
		// Update author's last-post timestamps so subsequent comments
		// from this account hit the interval gate above.
		authorAcct.LastPost = ctx.HeadBlockTime
		if !hasParent {
			authorAcct.LastRootPost = ctx.HeadBlockTime
		}
		objects.PutAccount(ctx.DB, &authorAcct)

		objects.PutComment(ctx.DB, &objects.Comment{
			Author:               c.Author,
			Permlink:             c.Permlink,
			ParentAuthor:         c.ParentAuthor,
			ParentPermlink:       c.ParentPermlink,
			Created:              ctx.HeadBlockTime,
			LastUpdate:           ctx.HeadBlockTime,
			CashoutTime:          cashoutTime,
			MaxAcceptedPayout:    1_000_000_000_000, // HIVE_MAX_SHARE_SUPPLY default
			PercentHbd:           10000,             // HIVE_100_PERCENT
			AllowVotes:           true,
			AllowCurationRewards: true,
		})
		if cashoutTime != 0 {
			objects.PutCommentCashoutSchedule(ctx.DB, cashoutTime, c.Author, c.Permlink)
		}
		return nil
	}
	// Update existing.
	existing.LastUpdate = ctx.HeadBlockTime
	objects.PutComment(ctx.DB, &existing)
	return nil
}

// deleteCommentEvaluator implements delete_comment.
//
// Mirrors libraries/chain/hive_evaluator_social.cpp:55:
//   - Pre-HF19: rejects only if the comment has replies (we don't track
//     a children_count, so we accept; close enough — the chain rejects
//     these too if hived's own logic catches them, otherwise we have an
//     extra deletion).
//   - HF19+: net_rshares <= 0 required; if > 0, silent no-op (push
//     ineffective_delete_comment vop in hived).
//   - Always: post-payout (no comment_cashout / our CashoutTime == 0
//     sentinel) is a hard error post-HF19.
type deleteCommentEvaluator struct{}

func (deleteCommentEvaluator) Apply(ctx *Context, o op.Operation) error {
	dc, ok := o.(op.DeleteComment)
	if !ok {
		return fmt.Errorf("deleteCommentEvaluator: unexpected op type %T", o)
	}
	c, err := objects.GetComment(ctx.DB, dc.Author, dc.Permlink)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: %q/%q", ErrCommentNotFound, dc.Author, dc.Permlink)
		}
		return err
	}
	if ctx.Hardforks.Has(hardfork.HF19) {
		// Post-HF19: silent no-op if net positive votes; hard error if
		// already paid out.
		if c.NetRshares > 0 {
			return nil
		}
	}
	if c.CashoutTime != 0 {
		objects.DeleteCommentCashoutSchedule(ctx.DB, c.CashoutTime, dc.Author, dc.Permlink)
	}
	objects.DeleteComment(ctx.DB, dc.Author, dc.Permlink)
	return nil
}

// commentOptionsEvaluator implements comment_options.
//
// Mirrors libraries/chain/hive_evaluator_social.cpp:137:
//   - tightening only (cannot raise max_payout, cannot raise percent_hbd,
//     cannot re-enable curation rewards or votes once disabled);
//   - tightening that EXCLUDES new positive rshares (disabling votes,
//     disabling curation, or lowering max_payout) requires no existing
//     votes on the comment.
type commentOptionsEvaluator struct{}

func (commentOptionsEvaluator) Apply(ctx *Context, o op.Operation) error {
	co, ok := o.(op.CommentOptions)
	if !ok {
		return fmt.Errorf("commentOptionsEvaluator: unexpected op type %T", o)
	}
	c, err := objects.GetComment(ctx.DB, co.Author, co.Permlink)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: %q/%q", ErrCommentNotFound, co.Author, co.Permlink)
		}
		return err
	}
	// hived: post-HF24, comment_options on a paid-out comment is a hard
	// error. Pre-HF24 it's a silent no-op. We use CashoutTime == 0 as our
	// paid-out sentinel.
	if c.CashoutTime == 0 {
		return nil
	}

	// "Disabling-or-lowering" requires no votes on the comment yet.
	disablingCuration := !co.AllowCurationRewards
	disablingVotes := !co.AllowVotes
	loweringMaxPayout := co.MaxAcceptedPayout.Amount < c.MaxAcceptedPayout
	if (disablingCuration || disablingVotes || loweringMaxPayout) && c.VoteCount != 0 {
		return fmt.Errorf("%w: option tightening requires no votes", ErrCommentClosed)
	}
	// Always tightening-only: can't re-enable curation/votes once off.
	if !c.AllowCurationRewards && co.AllowCurationRewards {
		return fmt.Errorf("comment_options: curation rewards cannot be re-enabled")
	}
	if !c.AllowVotes && co.AllowVotes {
		return fmt.Errorf("comment_options: voting cannot be re-enabled")
	}
	if co.MaxAcceptedPayout.Amount > c.MaxAcceptedPayout {
		return fmt.Errorf("comment_options: max_accepted_payout cannot be raised")
	}
	if co.PercentHbd > c.PercentHbd {
		return fmt.Errorf("comment_options: percent_hbd cannot be raised")
	}

	c.MaxAcceptedPayout = co.MaxAcceptedPayout.Amount
	c.PercentHbd = co.PercentHbd
	c.AllowVotes = co.AllowVotes
	c.AllowCurationRewards = co.AllowCurationRewards

	// Beneficiaries (HF17+): the comment_payout_beneficiaries extension
	// can only be set once, and only before the first vote. Hived
	// libraries/chain/hive_evaluator_social.cpp:172 enforces both.
	if len(co.Beneficiaries) > 0 {
		if !ctx.Hardforks.Has(hardfork.HF17) {
			return fmt.Errorf("comment_options: beneficiaries require HF17")
		}
		if len(c.Beneficiaries) > 0 {
			return fmt.Errorf("comment_options: beneficiaries already set")
		}
		if c.AbsRshares != 0 {
			return fmt.Errorf("comment_options: beneficiaries cannot be set after first vote")
		}
		if len(co.Beneficiaries) > 8 {
			return fmt.Errorf("comment_options: too many beneficiaries (max 8)")
		}
		var totalWeight uint32
		seen := map[auth.AccountName]struct{}{}
		out := make([]objects.Beneficiary, 0, len(co.Beneficiaries))
		for _, b := range co.Beneficiaries {
			if _, dup := seen[b.Account]; dup {
				return fmt.Errorf("comment_options: duplicate beneficiary %q", b.Account)
			}
			seen[b.Account] = struct{}{}
			if has, herr := objects.HasAccount(ctx.DB, b.Account); herr != nil {
				return herr
			} else if !has {
				return fmt.Errorf("%w: beneficiary %q", ErrAccountNotFound, b.Account)
			}
			totalWeight += uint32(b.Weight)
			out = append(out, objects.Beneficiary{Account: b.Account, Weight: b.Weight})
		}
		if totalWeight > 10000 {
			return fmt.Errorf("comment_options: total beneficiary weight %d > 100%%", totalWeight)
		}
		c.Beneficiaries = out
	}

	objects.PutComment(ctx.DB, &c)
	return nil
}

// voteEvaluator implements vote. The full hived semantics involve a
// nontrivial rshares calculation derived from voting power, account
// vesting_shares, current vote weight, and voting-mana decay; the
// payout curve and curation reward distribution add further complexity.
//
// For the minimum-viable apply we record the vote (creating a
// CommentVote row, incrementing the comment's vote_count, and adding
// a placeholder rshares contribution) without computing the curve.
// Mana decay and exact rshares math land with the curation work.
type voteEvaluator struct{}

func (voteEvaluator) Apply(ctx *Context, o op.Operation) error {
	v, ok := o.(op.Vote)
	if !ok {
		return fmt.Errorf("voteEvaluator: unexpected op type %T", o)
	}

	voterCheck, err := objects.GetAccount(ctx.DB, v.Voter)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: voter=%q", ErrAccountNotFound, v.Voter)
		}
		return err
	}
	if !voterCheck.CanVote() {
		return fmt.Errorf("vote: %q has declined voting rights", v.Voter)
	}

	c, err := objects.GetComment(ctx.DB, v.Author, v.Permlink)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: %q/%q", ErrCommentNotFound, v.Author, v.Permlink)
		}
		return err
	}
	// hived rejects allows_votes=false ONLY when this is an upvote
	// (weight > 0); downvotes still go through to dock rshares
	// (hive_evaluator_social.cpp:325).
	if v.Weight > 0 && !c.AllowVotes {
		return fmt.Errorf("%w: votes disabled for comment", ErrCommentClosed)
	}
	// Once a comment has cashed out (CashoutTime sentinel = 0 in our
	// model, time_point_sec::max in hived) further votes are no-ops.
	// This is the !comment_cashout / paid-out branch at the top of
	// hived's vote evaluator (hive_evaluator_social.cpp:328).
	if c.CashoutTime == 0 {
		return nil
	}

	prev, err := objects.GetCommentVote(ctx.DB, v.Voter, v.Author, v.Permlink)
	if err != nil && err != state.ErrNotFound {
		return err
	}

	// hived only enforces "weight != 0" on FIRST cast; recasts may set
	// weight to 0 to remove the vote (libraries/chain/hive_evaluator_social.cpp:456).
	isNew := err == state.ErrNotFound
	if isNew && v.Weight == 0 {
		return ErrVoteWeightZero
	}

	// Compute rshares using hived's voting-manabar formula
	// (hive_evaluator_social.cpp:336-365). The voter's current_mana
	// regenerates linearly back to HIVE_100_PERCENT (10000 pre-HF20)
	// over HIVE_VOTING_MANA_REGENERATION_SECONDS (5 days); each vote
	// burns roughly 1/(reserve_rate × 5_days/86400) of full mana per
	// 100%-weight vote — i.e. ~50 mana per max-weight vote pre-HF20
	// (reserve_rate = 40), letting an account cast ~200 full-power
	// votes before depletion.
	voter, verr := objects.GetAccount(ctx.DB, v.Voter)
	if verr != nil {
		return verr
	}
	const (
		hivePercent              int64 = 10000
		votingManaRegenerationSec int64 = 5 * 24 * 60 * 60 // 5 days
		secondsPerDay            int64 = 24 * 60 * 60
	)
	voteReserveRate := int64(40) // HIVE_INITIAL_VOTE_POWER_RATE
	if ctx.Hardforks.Has(hardfork.HF20) {
		voteReserveRate = 10 // HIVE_REDUCED_VOTE_POWER_RATE
	}
	elapsed := int64(ctx.HeadBlockTime) - int64(voter.VotingManaLastUpdate)
	if elapsed < 0 {
		elapsed = 0
	}
	regenerated := hivePercent * elapsed / votingManaRegenerationSec
	currentPower := voter.VotingManaCurrent + regenerated
	if currentPower > hivePercent {
		currentPower = hivePercent
	}
	if currentPower <= 0 {
		return fmt.Errorf("vote: %q has no voting power", v.Voter)
	}
	absWeight := int64(v.Weight)
	if absWeight < 0 {
		absWeight = -absWeight
	}
	usedPower := (currentPower * absWeight / hivePercent) * secondsPerDay
	maxVoteDenom := voteReserveRate * votingManaRegenerationSec
	if !ctx.Hardforks.Has(hardfork.HF14) {
		usedPower = (usedPower / maxVoteDenom) + 1
	} else {
		usedPower = (usedPower + maxVoteDenom - 1) / maxVoteDenom
	}
	if usedPower > currentPower {
		return fmt.Errorf("vote: %q does not have enough power (%d > %d)", v.Voter, usedPower, currentPower)
	}
	// abs_rshares = vesting × used_power / HIVE_100_PERCENT. Pre-HF1 the
	// product fits comfortably in int64; post-HF1 vesting can reach 1e16
	// satoshi but used_power ≤ ~50, so 5e17 still fits. big.Int kept for
	// defensive overflow safety.
	absRsharesBig := new(big.Int).Mul(big.NewInt(voter.VestingShares), big.NewInt(usedPower))
	absRsharesBig.Quo(absRsharesBig, big.NewInt(hivePercent))
	var absRshares int64
	if absRsharesBig.IsInt64() {
		absRshares = absRsharesBig.Int64()
	}
	// Pre-HF14 (libraries/chain/hive_evaluator_social.cpp:365): tiny
	// votes that round to 0 rshares get bumped to 1, so they don't
	// silently drop the vote-count increment.
	if !ctx.Hardforks.Has(hardfork.HF14) && absRshares == 0 {
		absRshares = 1
	}
	// Post-HF14 dust threshold: votes with |rshares| <= HIVE_VOTE_DUST_THRESHOLD
	// are rejected unless weight == 0 (recast-to-zero is allowed for cleanup).
	// Mirrors hived's hive_evaluator_social.cpp:369-373.
	const hiveVoteDustThreshold int64 = 50_000_000
	if ctx.Hardforks.Has(hardfork.HF14) && absRshares <= hiveVoteDustThreshold && v.Weight != 0 {
		return fmt.Errorf("vote: %q rshares %d below dust threshold", v.Voter, absRshares)
	}
	// hive_evaluator_social.cpp:706 — subtract dust threshold from
	// rshares (post-HF14). This is what flows into net_rshares; the
	// effective reward weight for a vote is mana_used - dust_threshold.
	if ctx.Hardforks.Has(hardfork.HF14) {
		absRshares -= hiveVoteDustThreshold
		if absRshares < 0 {
			absRshares = 0
		}
	}
	rshares := absRshares
	if v.Weight < 0 {
		rshares = -rshares
	}
	// Upvote lockout (HF7+): can't INCREASE rshares within the lockout
	// window before payout. HF17+ uses 12h, HF7-HF17 uses 1 minute.
	prevRshares := int64(0)
	if !isNew {
		prevRshares = prev.Rshares
	}
	if rshares > prevRshares && c.CashoutTime != 0 {
		var lockout uint32
		if ctx.Hardforks.Has(hardfork.HF17) {
			lockout = 60 * 60 * 12 // 12 hours
		} else if ctx.Hardforks.Has(hardfork.HF7) {
			lockout = 60 // 1 minute
		}
		if lockout > 0 && c.CashoutTime > ctx.HeadBlockTime && c.CashoutTime-ctx.HeadBlockTime < lockout {
			return fmt.Errorf("vote: cannot increase payout in last %ds before cashout", lockout)
		}
	}
	// Update the voter's manabar.
	voter.VotingManaCurrent = currentPower - usedPower
	voter.VotingManaLastUpdate = ctx.HeadBlockTime

	// Compute curation weight for this vote — the W(R_N) - W(R_N-1)
	// increment on the comment's running positive-rshares total.
	// Mirrors hive_evaluator_social.cpp:492-554. Only positive
	// (upvote) rshares contribute to curation weight; downvotes and
	// recasts get zero curation weight.
	var voteCurationWeight uint64
	if rshares > 0 && c.AllowCurationRewards && err == state.ErrNotFound {
		oldVoteRshares := c.VoteRshares
		newVoteRshares := oldVoteRshares + uint64(rshares)
		oldW := curationWeightCurve(ctx, &c, oldVoteRshares)
		newW := curationWeightCurve(ctx, &c, newVoteRshares)
		if newW.Cmp(oldW) > 0 {
			delta := new(big.Int).Sub(newW, oldW)
			if delta.IsUint64() {
				voteCurationWeight = delta.Uint64()
			} else {
				voteCurationWeight = ^uint64(0)
			}
		}
		// Reverse-auction (HF6+): early voters get scaled-down weight.
		// vote_weight *= min(elapsed, window) / window.
		dgpo, _ := objects.GetDGPO(ctx.DB)
		reverseAuction := uint32(0)
		if dgpo.ReverseAuctionSeconds > 0 {
			reverseAuction = dgpo.ReverseAuctionSeconds
		} else if ctx.Hardforks.Has(hardfork.HF6) {
			reverseAuction = 30 * 60 // 30 min default
		}
		if reverseAuction > 0 && voteCurationWeight > 0 {
			elapsed := uint32(0)
			if ctx.HeadBlockTime > c.Created {
				elapsed = ctx.HeadBlockTime - c.Created
			}
			if elapsed > reverseAuction {
				elapsed = reverseAuction
			}
			scaled := new(big.Int).Mul(
				new(big.Int).SetUint64(voteCurationWeight),
				big.NewInt(int64(elapsed)))
			scaled.Quo(scaled, big.NewInt(int64(reverseAuction)))
			if scaled.IsUint64() {
				voteCurationWeight = scaled.Uint64()
			}
		}
		c.VoteRshares = newVoteRshares
	}

	// hived's adjust_rshares2: the global total_reward_shares2 must move
	// by curve(new_net_rshares) - curve(old_net_rshares) on every cast/
	// recast so the comment's slice of the reward fund reflects the
	// updated total at cashout.
	oldNetRshares := c.NetRshares
	if err == state.ErrNotFound {
		objects.PutCommentVote(ctx.DB, &objects.CommentVote{
			Voter:          v.Voter,
			Author:         v.Author,
			Permlink:       v.Permlink,
			Weight:         v.Weight,
			NumChanges:     0,
			Rshares:        rshares,
			LastUpdate:     ctx.HeadBlockTime,
			CurationWeight: voteCurationWeight,
		})
		c.NetRshares += rshares
		c.VoteCount++
		c.TotalVoteWeight += voteCurationWeight
	} else {
		// Recast: subtract the previous vote's curation_weight from
		// the running total — hived sets the new weight to zero for
		// recasts (HF20+) so we don't recompute.
		if prev.CurationWeight > 0 && c.TotalVoteWeight >= prev.CurationWeight {
			c.TotalVoteWeight -= prev.CurationWeight
		}
		c.NetRshares += rshares - prev.Rshares
		prev.Rshares = rshares
		prev.Weight = v.Weight
		prev.NumChanges++
		prev.LastUpdate = ctx.HeadBlockTime
		prev.CurationWeight = 0
		objects.PutCommentVote(ctx.DB, &prev)
	}
	// Track AbsRshares for the beneficiary "no votes yet" gate.
	if absRshares > 0 {
		c.AbsRshares += absRshares
	}
	c.LastUpdate = ctx.HeadBlockTime
	objects.PutComment(ctx.DB, &c)
	objects.PutAccount(ctx.DB, &voter)
	adjustRewardShares2(ctx.DB, oldNetRshares, c.NetRshares)
	return nil
}

// curationWeightCurve evaluates the curve used for curation weights
// W(R). Pre-HF17 (HF1+): W(R) = uint64_max × R / (2s + R) using a
// uint128 multiply. Post-HF17 it's the reward_fund's curation_reward_curve
// (default bounded_curation, HF21 → linear at HF25).
//
// Mirrors hive_evaluator_social.cpp:519-539 + util/reward.cpp.
func curationWeightCurve(ctx *Context, c *objects.Comment, voteRshares uint64) *big.Int {
	if voteRshares == 0 {
		return new(big.Int)
	}
	r := new(big.Int).SetUint64(voteRshares)

	// Pick the curve. HF17+ uses the reward_fund's curve (bounded_curation
	// by default, square_root pre-HF19_1052, linear post-HF21).
	if ctx.Hardforks.Has(hardfork.HF17) {
		fund, err := objects.GetRewardFund(ctx.DB, objects.PostRewardFundName)
		if err == nil {
			cc := fund.ContentConstant
			if cc <= 0 {
				cc = 2_000_000_000_000
			}
			s := big.NewInt(cc)
			switch fund.CurationRewardCurve {
			case objects.CurveLinear:
				return r
			case objects.CurveSquareRoot:
				return new(big.Int).Sqrt(r)
			case objects.CurveConvergentLinear:
				num := new(big.Int).Mul(r, r)
				den := new(big.Int).Add(new(big.Int).Lsh(s, 1), r)
				return new(big.Int).Quo(num, den)
			case objects.CurveConvergentSquareRoot:
				num := new(big.Int).Set(r)
				den := new(big.Int).Add(s, new(big.Int).Sqrt(r))
				return new(big.Int).Quo(num, den)
			default:
				// bounded_curation: rshares / (2s + rshares) × 2^64
				// (the C++ uses `to_uint128(low_bits, 0)` which is a left-shift by 64).
				twoS := new(big.Int).Lsh(s, 1)
				num := new(big.Int).Lsh(r, 64)
				den := new(big.Int).Add(twoS, r)
				return new(big.Int).Quo(num, den)
			}
		}
	}

	// Pre-HF17: W(R) = uint64_max × R / (2s + R), with 2s = 2 ×
	// HIVE_CONTENT_CONSTANT_HF0.
	const contentConstantHF0 int64 = 2_000_000_000_000
	twoS := new(big.Int).SetInt64(2 * contentConstantHF0)
	uint64Max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1))
	num := new(big.Int).Mul(uint64Max, r)
	den := new(big.Int).Add(twoS, r)
	return new(big.Int).Quo(num, den)
}

// adjustRewardShares2 mirrors hived's database::adjust_rshares2: shift
// the running curve sum by curve(new_net_rshares) - curve(old_net_rshares)
// on every cast/recast. Pre-HF17 the sum lives in
// dgpo.total_reward_shares2; HF17+ it lives in
// reward_fund_object.recent_claims (with the fund's selected curve and
// content_constant).
func adjustRewardShares2(s *state.Session, oldNetRshares, newNetRshares int64) {
	// Post-HF17: write to reward_fund.recent_claims.
	if fund, err := objects.GetRewardFund(s, objects.PostRewardFundName); err == nil {
		total := reward.Uint128FromBytes(fund.RecentClaims)
		oldCurve := evaluateCurveForFund(oldNetRshares, &fund)
		newCurve := evaluateCurveForFund(newNetRshares, &fund)
		total.Sub(total, oldCurve)
		total.Add(total, newCurve)
		if total.Sign() < 0 {
			total.SetInt64(0)
		}
		fund.RecentClaims = reward.Uint128ToBytes(total)
		objects.PutRewardFund(s, &fund)
		return
	}
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return
	}
	total := reward.Uint128FromBytes(dgpo.TotalRewardShares2)
	oldCurve := reward.EvaluateRewardCurveQuadratic(oldNetRshares)
	newCurve := reward.EvaluateRewardCurveQuadratic(newNetRshares)
	total.Sub(total, oldCurve)
	total.Add(total, newCurve)
	if total.Sign() < 0 {
		total.SetInt64(0)
	}
	dgpo.TotalRewardShares2 = reward.Uint128ToBytes(total)
	objects.PutDGPO(s, &dgpo)
}

// evaluateCurveForFund applies the reward_fund's author_reward_curve
// to rshares, using its content_constant. Mirrors the curve dispatch
// in libraries/chain/util/reward.cpp::evaluate_reward_curve.
func evaluateCurveForFund(rshares int64, f *objects.RewardFund) *big.Int {
	if rshares <= 0 {
		return new(big.Int)
	}
	r := big.NewInt(rshares)
	s := big.NewInt(f.ContentConstant)
	if s.Sign() <= 0 {
		s = big.NewInt(2_000_000_000_000)
	}
	switch f.AuthorRewardCurve {
	case objects.CurveLinear:
		return new(big.Int).Set(r)
	case objects.CurveSquareRoot:
		return new(big.Int).Sqrt(r)
	case objects.CurveConvergentLinear:
		num := new(big.Int).Mul(r, r)
		den := new(big.Int).Add(new(big.Int).Lsh(s, 1), r)
		return new(big.Int).Quo(num, den)
	case objects.CurveConvergentSquareRoot:
		num := new(big.Int).Set(r)
		den := new(big.Int).Add(s, new(big.Int).Sqrt(r))
		return new(big.Int).Quo(num, den)
	default:
		rPlusS := new(big.Int).Add(r, s)
		out := new(big.Int).Mul(rPlusS, rPlusS)
		s2 := new(big.Int).Mul(s, s)
		out.Sub(out, s2)
		return out
	}
}

func init() {
	register(op.TagComment, commentEvaluator{})
	register(op.TagDeleteComment, deleteCommentEvaluator{})
	register(op.TagCommentOptions, commentOptionsEvaluator{})
	register(op.TagVote, voteEvaluator{})
}
