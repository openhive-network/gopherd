package evaluator

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

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

	existing, err := objects.GetComment(ctx.DB, c.Author, c.Permlink)
	if err != nil && err != state.ErrNotFound {
		return err
	}
	if err == state.ErrNotFound {
		// New comment.
		objects.PutComment(ctx.DB, &objects.Comment{
			Author:               c.Author,
			Permlink:             c.Permlink,
			ParentAuthor:         c.ParentAuthor,
			ParentPermlink:       c.ParentPermlink,
			Created:              ctx.BlockTime,
			LastUpdate:           ctx.BlockTime,
			MaxAcceptedPayout:    1_000_000_000_000, // HIVE_MAX_SHARE_SUPPLY default
			PercentHbd:           10000,             // HIVE_100_PERCENT
			AllowVotes:           true,
			AllowCurationRewards: true,
		})
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
	objects.DeleteComment(ctx.DB, dc.Author, dc.Permlink)
	return nil
}

// commentOptionsEvaluator implements comment_options. Sets the
// payout-time properties on the comment; rejected if the comment
// already has votes (matches hived).
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
	if c.VoteCount != 0 {
		return fmt.Errorf("%w: cannot change options after votes are cast", ErrCommentClosed)
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
	if v.Weight == 0 {
		return ErrVoteWeightZero
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
	// Placeholder curation/author reward.
	//
	// Hived's real flow accrues rshares over a 7-day cashout window then
	// pays curators+authors from the comment's reward_fund slice; that
	// pipeline (manabar decay, reward curve, virtual_supply tracking) is
	// not yet implemented. Skipping payouts entirely starves accounts
	// like steemit of VESTS they need to back later withdraw_vesting and
	// witness-vote-weight ops, which blocks replay.
	//
	// As a stopgap we credit a flat 0.010 HIVE worth of VESTS to the
	// voter and the author on positive votes, routed through CreateVesting
	// so fund_hive grows with shares and the dynamic price stays at the
	// 1000:1 satoshi-level ratio. Replace with the real cashout pipeline
	// once virtual_supply and reward_fund tracking land.
	if v.Weight > 0 {
		const placeholderHive int64 = 10 // 0.010 HIVE → 10K VESTS-satoshi
		if has, herr := objects.HasAccount(ctx.DB, v.Voter); herr == nil && has {
			_, _ = CreateVesting(ctx.DB, v.Voter, placeholderHive, false)
		}
		if v.Author != v.Voter {
			if has, herr := objects.HasAccount(ctx.DB, v.Author); herr == nil && has {
				_, _ = CreateVesting(ctx.DB, v.Author, placeholderHive, false)
			}
		}
	}

	if err == state.ErrNotFound {
		// First-time vote.
		objects.PutCommentVote(ctx.DB, &objects.CommentVote{
			Voter:      v.Voter,
			Author:     v.Author,
			Permlink:   v.Permlink,
			Weight:     v.Weight,
			NumChanges: 0,
			Rshares:    0, // computed by curation work
			LastUpdate: ctx.BlockTime,
		})
		c.VoteCount++
	} else {
		// Re-cast — update weight and num_changes.
		prev.Weight = v.Weight
		prev.NumChanges++
		prev.LastUpdate = ctx.BlockTime
		objects.PutCommentVote(ctx.DB, &prev)
	}
	c.LastUpdate = ctx.BlockTime
	objects.PutComment(ctx.DB, &c)
	return nil
}

func init() {
	register(op.TagComment, commentEvaluator{})
	register(op.TagDeleteComment, deleteCommentEvaluator{})
	register(op.TagCommentOptions, commentOptionsEvaluator{})
	register(op.TagVote, voteEvaluator{})
}
