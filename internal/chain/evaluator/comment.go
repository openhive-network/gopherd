package evaluator

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/reward"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Cashout windows mirror libraries/chain/hive_evaluator_social.cpp:248:
//
//	if has_hardfork(HF17__769):              cashout = now + 7d
//	elif !parent && has_hardfork(HF12__177): cashout = now + 12h
//	else:                                    cashout = time_point_sec::max  (no cashout)
//
// Pre-HF12 (block 3,533,567) NO comments pay out — content_reward
// accumulates unbounded in total_reward_fund_hive until HF12 kicks in.
// HF12 to HF17: only ROOT posts pay out (replies remain frozen). HF17+:
// all comments pay out at 7d.
const (
	cashoutWindowSecondsPreHF17 uint32 = 60 * 60 * 12
	cashoutWindowSeconds        uint32 = 60 * 60 * 24 * 7
)

// commentCashoutTime returns the cashout time for a new comment given
// the hardforks active at apply time and whether it has a parent (i.e.
// it's a reply, not a root post). Returns 0 to mean "never cash out"
// (we use 0 as our sentinel since uint32 can't hold time_point_sec::max).
func commentCashoutTime(hf hardfork.Set, blockTime uint32, hasParent bool) uint32 {
	if hf.Has(hardfork.HF17) {
		return blockTime + cashoutWindowSeconds
	}
	if !hasParent && hf.Has(hardfork.HF12) {
		return blockTime + cashoutWindowSecondsPreHF17
	}
	return 0 // never cash out
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
	if has, err := objects.HasAccount(ctx.DB, c.Author); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: author=%q", ErrAccountNotFound, c.Author)
	}

	// The parent must exist if this is a reply (parent_author non-empty).
	// hived's comment_evaluator (libraries/chain/hive_evaluator_social.cpp:184)
	// calls _db.get_comment(parent_author, parent_permlink) which throws if
	// not found. For root posts (parent_author == ""), the parent is the
	// implicit "category" wrapper so we skip the check.
	if c.ParentAuthor != "" {
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
	if err == state.ErrNotFound {
		// New comment — pre-HF12 comments are frozen (no cashout).
		// HF12-HF17 only root posts cash out. HF17+ all comments do.
		// commentCashoutTime returns 0 when the comment never cashes out.
		hasParent := c.ParentAuthor != ""
		cashoutTime := commentCashoutTime(ctx.Hardforks, ctx.BlockTime, hasParent)
		objects.PutComment(ctx.DB, &objects.Comment{
			Author:               c.Author,
			Permlink:             c.Permlink,
			ParentAuthor:         c.ParentAuthor,
			ParentPermlink:       c.ParentPermlink,
			Created:              ctx.BlockTime,
			LastUpdate:           ctx.BlockTime,
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
	existing.LastUpdate = ctx.BlockTime
	objects.PutComment(ctx.DB, &existing)
	return nil
}

// deleteCommentEvaluator implements delete_comment. hived only allows
// deletion if there are no votes and no children. We enforce the
// no-votes invariant; child counting requires the children_count field
// which lands with the full comment schema.
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
	if c.VoteCount != 0 {
		return fmt.Errorf("%w: comment has %d votes", ErrCommentClosed, c.VoteCount)
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

	if has, err := objects.HasAccount(ctx.DB, v.Voter); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: voter=%q", ErrAccountNotFound, v.Voter)
	}

	c, err := objects.GetComment(ctx.DB, v.Author, v.Permlink)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: %q/%q", ErrCommentNotFound, v.Author, v.Permlink)
		}
		return err
	}
	if !c.AllowVotes {
		return fmt.Errorf("%w: votes disabled for comment", ErrCommentClosed)
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

	// Compute rshares contribution. hived's real formula uses the voter's
	// current voting-power manabar, vesting shares, and the vote weight
	// percentage; we approximate by using `voter.VestingShares × weight /
	// HIVE_100_PERCENT` directly. This overstates rshares relative to
	// hived (which depletes voting power on each vote, recovering at
	// 20%/day), but the cashout rewards normalize within each comment so
	// the relative split among curators is unchanged.
	voter, verr := objects.GetAccount(ctx.DB, v.Voter)
	if verr != nil {
		return verr
	}
	rshares := voter.VestingShares * int64(v.Weight) / 10000

	// hived's adjust_rshares2: the global total_reward_shares2 must move
	// by curve(new_net_rshares) - curve(old_net_rshares) on every cast/
	// recast so the comment's slice of the reward fund reflects the
	// updated total at cashout.
	oldNetRshares := c.NetRshares
	if err == state.ErrNotFound {
		objects.PutCommentVote(ctx.DB, &objects.CommentVote{
			Voter:      v.Voter,
			Author:     v.Author,
			Permlink:   v.Permlink,
			Weight:     v.Weight,
			NumChanges: 0,
			Rshares:    rshares,
			LastUpdate: ctx.BlockTime,
		})
		c.NetRshares += rshares
		c.VoteCount++
	} else {
		c.NetRshares += rshares - prev.Rshares
		prev.Rshares = rshares
		prev.Weight = v.Weight
		prev.NumChanges++
		prev.LastUpdate = ctx.BlockTime
		objects.PutCommentVote(ctx.DB, &prev)
	}
	c.LastUpdate = ctx.BlockTime
	objects.PutComment(ctx.DB, &c)
	adjustRewardShares2(ctx.DB, oldNetRshares, c.NetRshares)
	return nil
}

// adjustRewardShares2 mirrors hived's database::adjust_rshares2: shift
// dgpo.total_reward_shares2 by the change in this comment's curve value.
// The curve is the pre-HF17 quadratic — `(rshares + s)² - s²`. We
// recompute both endpoints and apply the delta.
func adjustRewardShares2(s *state.Session, oldNetRshares, newNetRshares int64) {
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

func init() {
	register(op.TagComment, commentEvaluator{})
	register(op.TagDeleteComment, deleteCommentEvaluator{})
	register(op.TagCommentOptions, commentOptionsEvaluator{})
	register(op.TagVote, voteEvaluator{})
}
