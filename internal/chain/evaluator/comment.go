package evaluator

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"os"

	"hived-go/internal/math128"
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
//
// Mirrors hive_evaluator_social.cpp:247-253:
//   - HF17+: cashout_time = now + 7d
//   - HF12+ root post: cashout_time = now + 12h
//   - else (pre-HF12 ANY comment, or HF12-HF17 reply): cashout_time = max
//     (frozen at creation; pre-HF17 votes weight-average it down via
//     hive_evaluator_social.cpp:410-449).
//
// We use 0 as our "max/frozen" sentinel.
func commentCashoutTime(hf hardfork.Set, blockTime uint32, hasParent bool) uint32 {
	if hf.Has(hardfork.HF17) {
		return blockTime + cashoutWindowSeconds
	}
	if hf.Has(hardfork.HF12) && !hasParent {
		// HF12-HF17 root post: 12h.
		return blockTime + cashoutWindowSecondsPreHF17
	}
	// Pre-HF12 (any comment) or HF12-HF17 reply: frozen at creation;
	// the vote evaluator pulls cashout_time down via the weighted-avg
	// formula on every cast.
	return 0
}

// Errors specific to comment evaluators.
var (
	ErrCommentNotFound = errors.New("comment not found")
	ErrCommentExists   = errors.New("comment already exists at this permlink")
	ErrCommentClosed   = errors.New("comment is no longer accepting votes / edits")
	ErrVoteWeightZero  = errors.New("vote weight cannot be zero")
)

// GHIVEDIFF_VOTE_AUTHOR / GHIVEDIFF_VOTE_PERMLINK enable the per-vote
// GHIVEDIFF VOTE / VOTE-RECAST trace for a specific comment (every voter's
// rshares / curation_weight / eligibility). Permlink empty → all of the
// author's comments. Replaces the old hardcoded bitbo target.
var (
	voteDebugAuthor   = os.Getenv("GHIVEDIFF_VOTE_AUTHOR")
	voteDebugPermlink = os.Getenv("GHIVEDIFF_VOTE_PERMLINK")
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

func (commentEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagComment {
		return fmt.Errorf("commentEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	cp := opArg.AsComment()
	c := cp
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

		// reward_weight haircut for frequent posters (HF12-HF17 ROOT
		// posts only). Mirrors hive_evaluator_social.cpp:216-225.
		// Decays the previous post_bandwidth by the time-since-last-root
		// over a 1-day window, adds 10000 (HIVE_100_PERCENT), then
		// reward_weight = min(WEIGHT_CONSTANT × 100% / bw², 100%) where
		// WEIGHT_CONSTANT = (4 × 10000)² = 1,600,000,000. Applied at
		// cashout as `claim × reward_weight / 10000`. Stored on
		// comment_cashout_ex_object in C++; on Comment.RewardWeight here.
		const hivePostAverageWindow = uint64(60 * 60 * 24)
		const hive100Percent = uint64(10000)
		const hivePostWeightConstant = (4 * hive100Percent) * (4 * hive100Percent)
		var rewardWeight uint16 = uint16(hive100Percent)
		postBandwidth := uint64(authorAcct.PostBandwidth)
		if ctx.Hardforks.Has(hardfork.HF12) && !ctx.Hardforks.Has(hardfork.HF17) && !hasParent {
			postDeltaTime := uint64(ctx.HeadBlockTime - authorAcct.LastRootPost)
			if postDeltaTime > hivePostAverageWindow {
				postDeltaTime = hivePostAverageWindow
			}
			oldWeight := (postBandwidth * (hivePostAverageWindow - postDeltaTime)) / hivePostAverageWindow
			postBandwidth = oldWeight + hive100Percent
			rw := (hivePostWeightConstant * hive100Percent) / (postBandwidth * postBandwidth)
			if rw > hive100Percent {
				rw = hive100Percent
			}
			rewardWeight = uint16(rw)
		}

		// Update author's last-post timestamps so subsequent comments
		// from this account hit the interval gate above.
		authorAcct.LastPost = ctx.HeadBlockTime
		if !hasParent {
			authorAcct.LastRootPost = ctx.HeadBlockTime
			authorAcct.PostBandwidth = uint32(postBandwidth)
		}
		authorAcct.PostCount++
		objects.PutAccount(ctx.DB, &authorAcct)

		// Root-tracking: replies inherit Root from parent; root posts are
		// their own root. Mirrors comment_cashout_ex_object::root_id.
		rootAuthor := c.Author
		rootPermlink := c.Permlink
		if hasParent {
			if parent, perr := objects.GetComment(ctx.DB, c.ParentAuthor, c.ParentPermlink); perr == nil {
				if parent.RootAuthor != "" {
					rootAuthor = parent.RootAuthor
					rootPermlink = parent.RootPermlink
				} else {
					// Legacy parent without Root* set: parent's root is
					// itself if it's a top-level post, else trace up.
					rootAuthor = parent.Author
					rootPermlink = parent.Permlink
				}
				// on_reply: bump parent's children counter (only if its
				// cashout row still exists — pre-HF19 always; post-HF19
				// only while not yet paid out, mirrored by CashoutTime!=0).
				if parent.CashoutTime != 0 {
					parent.Children++
					objects.PutComment(ctx.DB, &parent)
				}
			}
		}

		// Allocate a monotonic comment ID from dgpo so the cashout
		// schedule can tie-break same-time entries by insertion order
		// (mirrors C++'s comment_object::id auto-increment).
		commentID := uint64(0)
		objects.MutateDGPO(ctx.DB, func(p *objects.DynamicGlobalProperty) {
			commentID = p.NextCommentID
			p.NextCommentID++
		})
		objects.PutComment(ctx.DB, &objects.Comment{
			Author:         c.Author,
			Permlink:       c.Permlink,
			ParentAuthor:   c.ParentAuthor,
			ParentPermlink: c.ParentPermlink,
			Created:        ctx.HeadBlockTime,
			LastUpdate:     ctx.HeadBlockTime,
			CashoutTime:    cashoutTime,
			// C++ comment_cashout_object initialiser: HBD_asset(1'000'000'000)
			// = 1,000,000.000 HBD (3 decimals). Mirrors comment_options_operation's
			// own default of `asset(1'000'000'000, HBD_SYMBOL)`. Using the
			// 1T sat-HBD HIVE_MAX_SHARE_SUPPLY value here makes every
			// subsequent comment_options op count as `lowering`, falsely
			// tripping the no-votes-yet tightening assert (block 4,972,630
			// freebornangel reproducer).
			MaxAcceptedPayout:    1_000_000_000,
			PercentHbd:           10000, // HIVE_100_PERCENT
			AllowVotes:           true,
			AllowCurationRewards: true,
			RootAuthor:           rootAuthor,
			RootPermlink:         rootPermlink,
			ID:                   commentID,
			RewardWeight:         rewardWeight,
		})
		if cashoutTime != 0 {
			objects.PutCommentCashoutSchedule(ctx.DB, cashoutTime, commentID, c.Author, c.Permlink)
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

func (deleteCommentEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagDeleteComment {
		return fmt.Errorf("deleteCommentEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	dc := opArg.AsDeleteComment()
	c, err := objects.GetComment(ctx.DB, dc.Author, dc.Permlink)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: %q/%q", ErrCommentNotFound, dc.Author, dc.Permlink)
		}
		return err
	}
	// hived's delete_comment_evaluator (hive_evaluator_social.cpp:55-106)
	// asserts a comment_cashout_object exists ("Cannot delete comment
	// after payout."). Pre-HF19, find_comment_cashout always returns a
	// valid object (the cashout row is rescheduled-or-frozen but never
	// removed) so the assert never fires. HF19+ removes the cashout
	// row at payout (database_comment.cpp:480), so deletes after payout
	// trip the assert. Our equivalent: c.CashoutTime==0 = frozen, but
	// only HF19+ should reject.
	if ctx.Hardforks.Has(hardfork.HF19) && c.WasPaid && c.CashoutTime == 0 {
		return fmt.Errorf("%w: cannot delete comment after payout", ErrCommentClosed)
	}
	// "Cannot delete a comment with replies." (hive_evaluator_social.cpp:60).
	// Pre-HF19 the cashout row is never removed, so the assert is always
	// active. HF19+ the cashout row goes away at payout, gating the check
	// behind CashoutTime != 0 (we already returned above when it's 0).
	if c.Children > 0 {
		return fmt.Errorf("%w: cannot delete comment with replies", ErrCommentClosed)
	}
	// NetRshares > 0 handling (hive_evaluator_social.cpp:67-75):
	//   - Post-HF19_977 (== HF19): hardfork-assert "Cannot delete a comment
	//     with net positive votes" (throws an error).
	//   - Pre-HF19:                silent no-op (push ineffective_delete vop).
	if c.NetRshares > 0 {
		if ctx.Hardforks.Has(hardfork.HF19) {
			return fmt.Errorf("%w: cannot delete comment with net positive votes", ErrCommentClosed)
		}
		return nil
	}
	if c.CashoutTime != 0 {
		objects.DeleteCommentCashoutSchedule(ctx.DB, c.CashoutTime, c.ID)
	}
	// C++ delete_comment_evaluator (hive_evaluator_social.cpp:77-85)
	// removes ALL comment_vote rows for this comment. Without this, an
	// author who deletes then re-creates a comment with the same
	// (author, permlink) sees a stale cv from the deleted comment when
	// they vote on the new one — our same-percent gate (HF6+) then
	// rejects it as "already voted in a similar way" though C++ accepts
	// it as a fresh first-cast.
	deleteAllCommentVotes(ctx.DB, dc.Author, dc.Permlink)
	// Decrement parent.Children when removing a reply (on_reply with deleting=true).
	if c.ParentAuthor != "" {
		if parent, perr := objects.GetComment(ctx.DB, c.ParentAuthor, c.ParentPermlink); perr == nil {
			if parent.Children > 0 {
				parent.Children--
				objects.PutComment(ctx.DB, &parent)
			}
		}
	}
	objects.DeleteComment(ctx.DB, dc.Author, dc.Permlink)
	return nil
}

// deleteAllCommentVotes drops every CommentVote on (author, permlink)
// in one shot. Used by delete_comment_evaluator's cleanup path.
func deleteAllCommentVotes(s *state.Session, author, permlink string) {
	objects.DeleteAllCommentVotesForComment(s, auth.AccountName(author), permlink)
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

func (commentOptionsEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagCommentOptions {
		return fmt.Errorf("commentOptionsEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	co := opArg.AsCommentOptions()
	c, err := objects.GetComment(ctx.DB, co.Author, co.Permlink)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: %q/%q", ErrCommentNotFound, co.Author, co.Permlink)
		}
		return err
	}
	// hived: post-HF24, comment_options on a paid-out comment is a hard
	// error (hive_evaluator_social.cpp:149). Pre-HF24 it's a silent
	// no-op. WasPaid + CashoutTime==0 is our "fully paid out / cashout
	// row removed" sentinel — distinct from "frozen at creation,
	// awaiting first vote" (CashoutTime==0 pre-HF12 / for replies, but
	// WasPaid=false).
	if c.WasPaid && c.CashoutTime == 0 {
		if ctx.Hardforks.Has(hardfork.HF24) {
			return fmt.Errorf("comment_options: cannot update parameters for paid-out comment")
		}
		return nil
	}

	// "Disabling-or-lowering" requires the comment to have no rshares
	// allocated yet. hived gates on comment_cashout->has_votes() ==
	// was_voted_on (hive_evaluator_social.cpp:155-156), a STICKY flag set by
	// on_vote when real_vote is true — every vote pre-HF20 and at HF26+, but
	// only abs_rshares != 0 votes in HF20..HF26 (dust votes excluded). Go
	// tracks this as c.WasVotedOn. VoteCount over-counts dust votes (wrong
	// post-HF20: block 43,550,866); AbsRshares skips dust votes (wrong
	// pre-HF20: block 19,236,724). WasVotedOn is correct across all forks.
	disablingCuration := !co.AllowCurationRewards
	disablingVotes := !co.AllowVotes
	loweringMaxPayout := co.MaxAcceptedPayout.Amount < c.MaxAcceptedPayout
	if (disablingCuration || disablingVotes || loweringMaxPayout) && c.WasVotedOn {
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

// voteEvaluator implements vote with the full hived semantics:
// voting-mana regeneration, used-mana → rshares (per-HF formula),
// reverse-auction discount + HF21+ early/mid/late voting phases, the
// per-HF curve dispatch (quadratic/bounded/sqrt/convergent_*) with
// approx_sqrt matching fc::uint128_approx_sqrt, recast handling (with
// HF20+ VoteRshares update), and pre-HF17 reward-shares-2 plus
// post-HF17 reward_fund.recent_claims accumulation.
//
// Pre-HF20 mana stays in percent units; HF20+ manabar refactor is
// tracked as task #53 (BLOCKER for replay past block 26.2M).
type voteEvaluator struct{}

func (voteEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagVote {
		return fmt.Errorf("voteEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	v := opArg.AsVote()
	// Dual GetComment/GetCommentRef path: under !UndoEnabled (replay)
	// c aliases the in-place CommentStore slot — read/write through
	// the pointer with no value-copy on Get and no PutComment at the
	// end. Under UndoEnabled (live sync / tests) we fall back to a
	// value-copy + slot-swap on PutComment so the undoLog captures the
	// pre-mutation snap.
	var cLocal objects.Comment
	var c *objects.Comment
	if ctx.DB.Database().UndoEnabled {
		var err error
		cLocal, err = objects.GetComment(ctx.DB, v.Author, v.Permlink)
		if err != nil {
			if err == state.ErrNotFound {
				return fmt.Errorf("%w: %q/%q", ErrCommentNotFound, v.Author, v.Permlink)
			}
			return err
		}
		c = &cLocal
	} else {
		var ok bool
		c, ok = objects.GetCommentRef(ctx.DB, v.Author, v.Permlink)
		if !ok {
			return fmt.Errorf("%w: %q/%q", ErrCommentNotFound, v.Author, v.Permlink)
		}
	}
	// hived rejects allows_votes=false ONLY when this is an upvote
	// (weight > 0); downvotes still go through to dock rshares
	// (hive_evaluator_social.cpp:325).
	if v.Weight > 0 && !c.AllowVotes {
		return fmt.Errorf("%w: votes disabled for comment", ErrCommentClosed)
	}
	// hive_evaluator_social.cpp:328: skip the vote when comment_cashout
	// is gone OR (HF12+ AND payout_time == max). C++'s
	// calculate_discussion_payout_time returns the ROOT's cashout_time
	// for pre-HF17 replies (database_comment.cpp:60-66), so pre-HF17
	// the gate uses ROOT's CashoutTime, not the reply's own.
	// Without this, Go silently skips votes on paid REPLIES whose ROOT
	// is still active (yuliana case at block 3,538,189) — which over-
	// preserves mana for the voter and drifts abs_rshares ~2% by the
	// next root vote, cascading into wrong cashout-time pulls.
	if ctx.Hardforks.Has(hardfork.HF12) {
		gateCashoutTime := c.CashoutTime
		gateWasPaid := c.WasPaid
		if !ctx.Hardforks.Has(hardfork.HF17) && c.ParentAuthor != "" {
			// pre-HF17 reply: payout-time gate uses the ROOT's state.
			rootAuthor := c.RootAuthor
			rootPermlink := c.RootPermlink
			if rootAuthor == "" {
				rootAuthor = c.Author
				rootPermlink = c.Permlink
			}
			if rootAuthor != c.Author || rootPermlink != c.Permlink {
				if root, rerr := objects.GetComment(ctx.DB, rootAuthor, rootPermlink); rerr == nil {
					gateCashoutTime = root.CashoutTime
					gateWasPaid = root.WasPaid
				}
			}
		}
		if gateWasPaid && gateCashoutTime == 0 {
			return nil
		}
	}

	prev, prevExists := objects.GetCommentVoteRef(ctx.DB, v.Voter, v.Author, v.Permlink)
	isNew := !prevExists

	// hived only enforces "weight != 0" on FIRST cast post-HF20.
	// The HF20 vote evaluator at hive_evaluator_social.cpp:638 has
	// the assert; the pre-HF20 evaluator at :314+ does not — pre-HF20
	// a zero-weight first-cast just falls through (with the
	// abs_rshares dust check exempting it at :369).
	if isNew && v.Weight == 0 && ctx.Hardforks.Has(hardfork.HF20) {
		return ErrVoteWeightZero
	}
	// HF6+ rejects recasts with the same percent as the prior vote
	// (hive_evaluator_social.cpp:592-593, "You have already voted in
	// a similar way."). Requires cv-delete fix at cashout to be in
	// place — otherwise stale cv rows trigger false same-percent
	// rejections on later votes that C++ treats as fresh.
	if !isNew && ctx.Hardforks.Has(hardfork.HF6) && prev.Weight == v.Weight {
		return fmt.Errorf("vote: %q has already voted in a similar way", v.Voter)
	}

	// Compute rshares using hived's voting-manabar formula
	// (hive_evaluator_social.cpp:336-365). The voter's current_mana
	// regenerates linearly back to HIVE_100_PERCENT (10000 pre-HF20)
	// over HIVE_VOTING_MANA_REGENERATION_SECONDS (5 days); each vote
	// burns roughly 1/(reserve_rate × 5_days/86400) of full mana per
	// 100%-weight vote — i.e. ~50 mana per max-weight vote pre-HF20
	// (reserve_rate = 40), letting an account cast ~200 full-power
	// votes before depletion.
	// Dual path: under SkipTxSavepoints (replay, UndoEnabled=false) we
	// mutate the AccountStore slot directly through a *Account pointer
	// — no value-copy on Get, no PutAccount at the end. Under
	// UndoEnabled=true (live sync / tests) we fall back to a value-copy
	// + slot-swap so the undoLog captures the correct pre-mutation
	// snap. Account is ~700 bytes; voteEvaluator fires 9.5M times on
	// an 8M run, so the read+write copy pair was ~13 GB of memcpy.
	var voterLocal objects.Account
	var voter *objects.Account
	if ctx.DB.Database().UndoEnabled {
		var verr error
		voterLocal, verr = objects.GetAccount(ctx.DB, v.Voter)
		if verr != nil {
			if verr == state.ErrNotFound {
				return fmt.Errorf("%w: voter=%q", ErrAccountNotFound, v.Voter)
			}
			return verr
		}
		voter = &voterLocal
	} else {
		var ok bool
		voter, ok = objects.GetAccountRef(ctx.DB, v.Voter)
		if !ok {
			return fmt.Errorf("%w: voter=%q", ErrAccountNotFound, v.Voter)
		}
	}
	if !voter.CanVote() {
		return fmt.Errorf("vote: %q has declined voting rights", v.Voter)
	}
	const (
		hivePercent               int64 = 10000
		votingManaRegenerationSec int64 = 5 * 24 * 60 * 60 // 5 days
		secondsPerDay             int64 = 24 * 60 * 60
	)
	// HIVE_INITIAL_VOTE_POWER_RATE = 40 pre-HF19; C++ database_hardfork.cpp
	// HF19 case sets `gpo.vote_power_reserve_rate = HIVE_REDUCED_VOTE_POWER_RATE`
	// (= 10) at HF19, NOT HF20. Earlier code gated on HF20 — wrong for the
	// HF19–HF20 window (block ~13M–~25M) where C++ already drops to 10
	// while Go was still at 40. Dormant for blocks pre-HF19 (we're at 5.2M
	// in this iteration), but a real divergence past HF19.
	voteReserveRate := int64(40) // HIVE_INITIAL_VOTE_POWER_RATE
	if ctx.Hardforks.Has(hardfork.HF19) {
		voteReserveRate = 10 // HIVE_REDUCED_VOTE_POWER_RATE
	}
	absWeight := int64(v.Weight)
	if absWeight < 0 {
		absWeight = -absWeight
	}
	// HF20+ manabar refactor: voting_manabar is in VESTS units, regen
	// caps at effective_vesting (excl withdraw). Pre-HF20 path stays
	// in percent units (0..10000) with cap = hivePercent.
	maxVoteDenom := voteReserveRate * votingManaRegenerationSec
	var currentPower int64
	var usedPower int64
	var dvPoolPct int64
	if ctx.Hardforks.Has(hardfork.HF20) {
		// hf20_vote_evaluator:627 — a vote at or past the comment's
		// cashout_time is rejected ("Comment is actively being
		// rewarded"); the paid case already returned nil above.
		if ctx.HeadBlockTime >= c.CashoutTime {
			return fmt.Errorf("vote: comment is actively being rewarded, cannot vote")
		}
		dgpoRef, _ := objects.GetDGPORef(ctx.DB)
		if dgpoRef != nil {
			dvPoolPct = int64(dgpoRef.DownvotePoolPercent) // 0 until HF21
		}
		// util::update_manabar(dgpo, a) — proper regen of both bars
		// (128-bit, dt capped at regen_time, clamped to the effective-
		// vesting cap). No bump. Note C++ has no "current_mana > 0"
		// assert here: a zero-mana vote proceeds and lands with
		// rshares=0 after the dust subtraction.
		objects.UpdateVotingManabar(voter, ctx.HeadBlockTime, uint16(dvPoolPct), 0, ctx.BlockNum)
		currentPower = voter.VotingManaCurrent
		// Base mana for the cost formula (hive_evaluator_social.cpp:
		// 665-684). Downvotes with an active pool (HF21+) price against
		// max(downvote_mana×10000/pool_pct, voting_mana); otherwise the
		// voting bar alone.
		manaForCalc := currentPower
		if ctx.Hardforks.Has(hardfork.HF28) {
			// HIVE_HARDFORK_1_28_STABLE_VOTE (hive_evaluator_social.cpp:660):
			// vote strength uses voter.get_effective_vesting_shares() —
			// whose default excludeWeeklyPowerDown=true EXCLUDES the next
			// power-down slice (same basis as the manabar cap). Using the
			// basic variant overcharged every powering-down voter by
			// 2%×weight×(weekly slice) per vote; guitarmcy's mana ran
			// ~83G short of hived's within 2,500 post-HF28 blocks
			// (false "not enough power" at 101,322,426). rshares become
			// stake-based (independent of manabar depletion) and bypass
			// the downvote-pool scaling; the manabar is still
			// regenerated + consumed below.
			manaForCalc = voter.EffectiveVestingSharesExclWithdraw()
		} else if dvPoolPct > 0 && v.Weight < 0 {
			var scaled int64
			if ctx.Hardforks.Has(hardfork.HF22) {
				// HF22__3485: widen to 128-bit before ×10000.
				hi, lo := bits.Mul64(uint64(voter.DownvoteManaCurrent), uint64(hivePercent))
				q, _ := bits.Div64(hi, lo, uint64(dvPoolPct))
				scaled = int64(q)
			} else {
				// Pre-HF22 C++ multiplies in int64 first (the overflow
				// this sub-fork fixed); mirror it bug-for-bug.
				scaled = (voter.DownvoteManaCurrent * hivePercent) / dvPoolPct
			}
			if scaled > manaForCalc {
				manaForCalc = scaled
			}
		}
		// used_mana = mana × abs_weight × 86400 / HIVE_100_PERCENT,
		// then ceil-divided by max_vote_denom = reserve_rate × regen_sec.
		// 128-bit since mana ≤ ~4e17 and the product overflows int64.
		if maxVoteDenom <= 0 {
			return fmt.Errorf("vote: max_vote_denom is zero")
		}
		// Step 1: mana × abs_weight × 86400 / 10000.
		hiM1, loM1 := bits.Mul64(uint64(manaForCalc), uint64(absWeight*secondsPerDay))
		// 128-bit / 64-bit yielding 128-bit quotient. We do this via two
		// Div64s: q_hi = hi/d; remainder feeds into the low half so
		// q_lo = (rem<<64 + lo) / d.
		hiDiv := hiM1 / uint64(hivePercent)
		hiRem := hiM1 % uint64(hivePercent)
		q1Lo, _ := bits.Div64(hiRem, loM1, uint64(hivePercent))
		q1Hi := hiDiv
		// Step 2: (q1 + denom-1) / denom.
		var carry uint64
		newLo, carry := bits.Add64(q1Lo, uint64(maxVoteDenom-1), 0)
		q1Hi += carry
		q2, _ := bits.Div64(q1Hi, newLo, uint64(maxVoteDenom))
		usedPower = int64(q2)
		if usedPower < 0 {
			usedPower = math.MaxInt64
		}
		// Mana sufficiency (hive_evaluator_social.cpp:690-706): a pooled
		// downvote checks voting+downvote STRICTLY greater than the cost;
		// everything else checks the voting bar alone (has_mana).
		if dvPoolPct > 0 && v.Weight < 0 {
			if voter.VotingManaCurrent+voter.DownvoteManaCurrent <= usedPower {
				return fmt.Errorf("vote: %q does not have enough mana to downvote (%d+%d <= %d)", v.Voter, voter.VotingManaCurrent, voter.DownvoteManaCurrent, usedPower)
			}
		} else if usedPower > 0 && usedPower > currentPower {
			return fmt.Errorf("vote: %q does not have enough power (%d > %d)", v.Voter, usedPower, currentPower)
		}
	} else {
		elapsed := int64(ctx.HeadBlockTime) - int64(voter.VotingManaLastUpdate)
		if elapsed < 0 {
			elapsed = 0
		}
		regenerated := hivePercent * elapsed / votingManaRegenerationSec
		currentPower = voter.VotingManaCurrent + regenerated
		if currentPower > hivePercent {
			currentPower = hivePercent
		}
		if currentPower <= 0 {
			return fmt.Errorf("vote: %q has no voting power", v.Voter)
		}
		usedPower = (currentPower * absWeight / hivePercent) * secondsPerDay
		if !ctx.Hardforks.Has(hardfork.HF14) {
			usedPower = (usedPower / maxVoteDenom) + 1
		} else {
			usedPower = (usedPower + maxVoteDenom - 1) / maxVoteDenom
		}
		if usedPower > currentPower {
			return fmt.Errorf("vote: %q does not have enough power (%d > %d)", v.Voter, usedPower, currentPower)
		}
	}
	// abs_rshares = effective_vesting_shares × used_power / HIVE_100_PERCENT
	// (hive_evaluator_social.cpp:364, :722).
	//
	// C++ has TWO variants of get_effective_vesting_shares:
	//   - false: vesting - delegated + received (the basic effective set)
	//   - true:  same MINUS get_next_vesting_withdrawal() (the "exclude
	//            weekly power-down" set)
	//
	// Pre-HF20 vote_evaluator calls the FALSE variant (line 364).
	// HF20+ hf20_vote_evaluator calls update_manabar which uses the
	// TRUE variant (manabar.hpp:107). Excluding the next-withdrawal
	// slice prevents VESTS about to leave the vesting pool from
	// inflating vote weight.
	var effectiveVesting int64
	if ctx.Hardforks.Has(hardfork.HF20) {
		effectiveVesting = voter.EffectiveVestingSharesExclWithdraw()
	} else {
		effectiveVesting = voter.EffectiveVestingShares()
	}
	// abs_rshares: at HF20+ usedPower IS already VESTS-scale (current_mana
	// is in VESTS), so abs_rshares = usedPower directly. Pre-HF20 we still
	// scale through effective_vesting * usedPower_pct / hive_percent.
	var absRshares int64
	if ctx.Hardforks.Has(hardfork.HF20) {
		absRshares = usedPower
		_ = effectiveVesting
	} else {
		hi, lo := bits.Mul64(uint64(effectiveVesting), uint64(usedPower))
		q, _ := bits.Div64(hi, lo, uint64(hivePercent))
		if q <= uint64(math.MaxInt64) {
			absRshares = int64(q)
		} else {
			absRshares = math.MaxInt64
		}
	}
	// Pre-HF14 (libraries/chain/hive_evaluator_social.cpp:365): tiny
	// votes that round to 0 rshares get bumped to 1, so they don't
	// silently drop the vote-count increment.
	if !ctx.Hardforks.Has(hardfork.HF14) && absRshares == 0 {
		absRshares = 1
	}
	// Post-HF14 dust threshold: votes with |rshares| <= HIVE_VOTE_DUST_THRESHOLD
	// are rejected unless weight == 0 (recast-to-zero is allowed for cleanup).
	// Mirrors hived's hive_evaluator_social.cpp:369-373. HF20+ has NO dust
	// rejection — hf20_vote_evaluator subtracts the threshold and clamps to 0
	// instead (the vote proceeds, consuming mana, with rshares possibly 0).
	const hiveVoteDustThreshold int64 = 50_000_000
	if ctx.Hardforks.Has(hardfork.HF14) && !ctx.Hardforks.Has(hardfork.HF20) &&
		absRshares <= hiveVoteDustThreshold && v.Weight != 0 {
		return fmt.Errorf("vote: %q rshares %d below dust threshold", v.Voter, absRshares)
	}
	// HF13-only dust assertion (hive_evaluator_social.cpp:371-373):
	// post-HF13_248, pre-HF14, hived rejects abs_rshares in [2, 50M] —
	// but explicitly allows abs_rshares == 1 (the legacy single-sat
	// edge case). Gopherd previously only gated on HF14+, so the
	// HF13→HF14 window (blocks ~3.3M-5.0M) accepted votes hived
	// rejected.
	if ctx.Hardforks.Has(hardfork.HF13) && !ctx.Hardforks.Has(hardfork.HF14) &&
		absRshares <= hiveVoteDustThreshold && absRshares != 1 && v.Weight != 0 {
		return fmt.Errorf("vote: %q rshares %d below HF13 dust threshold (allowed values: 1 or >50M)", v.Voter, absRshares)
	}
	// HF20+ subtracts the dust threshold from abs_rshares before it becomes
	// rshares (hf20_vote_evaluator at hive_evaluator_social.cpp:706). The
	// pre-HF20 evaluator only gates on the threshold (line 369) and uses
	// the full abs_rshares — so subtracting here too would shrink every
	// pre-HF20 vote by 50M rshares and starve every cashout downstream.
	if ctx.Hardforks.Has(hardfork.HF20) {
		absRshares -= hiveVoteDustThreshold
		if absRshares < 0 {
			absRshares = 0
		}
		// hf20_vote_evaluator:712-717 — within the last 12h before
		// cashout, rshares scale linearly toward 0 instead of the
		// pre-HF20 lockout REJECTION (which no longer applies at HF20+).
		// cashout_delta ≥ 1 is guaranteed by the now < cashout_time
		// reject above.
		const upvoteLockoutSec = 12 * 60 * 60
		cashoutDelta := int64(c.CashoutTime) - int64(ctx.HeadBlockTime)
		if cashoutDelta < upvoteLockoutSec {
			hi, lo := bits.Mul64(uint64(absRshares), uint64(cashoutDelta))
			q, _ := bits.Div64(hi, lo, uint64(upvoteLockoutSec))
			absRshares = int64(q)
		}
	}
	rshares := absRshares
	if v.Weight < 0 {
		rshares = -rshares
	}
	traceVoterMana := envGhivediff != "" && envGhivediff == string(v.Voter)
	if os.Getenv("GHIVEDIFF_VOTE_AUTHOR") != "" && string(v.Author) == os.Getenv("GHIVEDIFF_VOTE_AUTHOR") {
		pl := os.Getenv("GHIVEDIFF_VOTE_PERMLINK")
		if pl == "" || string(v.Permlink) == pl {
			traceVoterMana = true
		}
	}
	if traceVoterMana {
		fmt.Fprintf(os.Stderr, "GHIVEDIFF VOTERMANA block=%d voter=%s author=%s permlink=%s current_mana=%d max_mana=%d last_update=%d vests=%d recv=%d deleg=%d rshares=%d weight=%d\n",
			ctx.BlockNum, v.Voter, v.Author, v.Permlink, currentPower, voter.EffectiveVestingSharesExclWithdraw(), voter.VotingManaLastUpdate,
			voter.VestingShares, voter.ReceivedVestingShares, voter.DelegatedVestingShares, rshares, v.Weight)
	}
	// Upvote lockout (HF7+): can't INCREASE rshares within the lockout
	// window before payout. HF17+ uses 12h, HF7-HF17 uses 1 minute.
	// C++ hive_evaluator_social.cpp:396-397 uses calculate_discussion_payout_time
	// which for pre-HF17 replies returns the ROOT's cashout_time
	// (database_comment.cpp:60-66 follows the parent chain). Match that so
	// reply votes near root's cashout boundary are rejected as in C++.
	prevRshares := int64(0)
	if !isNew {
		prevRshares = prev.Rshares
	}
	if rshares > prevRshares && !c.WasPaid && !ctx.Hardforks.Has(hardfork.HF20) {
		// hived's lockout check runs ONLY on comments that have a live
		// comment_cashout_object (find_comment_cashout != nullptr). For
		// already-paid comments the object is deleted in hived, so the
		// check is skipped entirely and the late vote is silently
		// allowed (curation weight stays 0 per the was_paid gate).
		// Mirror that by gating on !c.WasPaid; first failing mainnet
		// hit was block 10,629,464 (vote on a comment cashed out 1 week
		// earlier).
		lockoutCashoutTime := c.CashoutTime
		if !ctx.Hardforks.Has(hardfork.HF17) && c.ParentAuthor != "" {
			rootAuthor := c.RootAuthor
			rootPermlink := c.RootPermlink
			if rootAuthor == "" {
				rootAuthor = c.Author
				rootPermlink = c.Permlink
			}
			if rootAuthor != c.Author || rootPermlink != c.Permlink {
				if root, rerr := objects.GetComment(ctx.DB, rootAuthor, rootPermlink); rerr == nil {
					lockoutCashoutTime = root.CashoutTime
				}
			}
		}
		if lockoutCashoutTime != 0 {
			var lockout uint32
			if ctx.Hardforks.Has(hardfork.HF17) {
				lockout = 60 * 60 * 12 // 12 hours
			} else if ctx.Hardforks.Has(hardfork.HF7) {
				lockout = 60 // 1 minute
			}
			if lockout > 0 && lockoutCashoutTime > ctx.HeadBlockTime && lockoutCashoutTime-ctx.HeadBlockTime <= lockout {
				return fmt.Errorf("vote: cannot increase payout in last %ds before cashout", lockout)
			}
		}
	}
	// Consume the mana (hive_evaluator_social.cpp:719-746). The
	// downvote bar participates ONLY while dgpo.downvote_pool_percent
	// is non-zero (HF21+; it is 0 for the whole HF20→HF21 window, so
	// HF20-era downvotes draw from the voting bar like upvotes). Both
	// bars were already regenerated by UpdateVotingManabar above, which
	// also stamped the last-update times.
	if ctx.Hardforks.Has(hardfork.HF20) {
		if dvPoolPct > 0 && v.Weight < 0 {
			if usedPower > voter.DownvoteManaCurrent {
				remainder := usedPower - voter.DownvoteManaCurrent
				voter.DownvoteManaCurrent = 0
				voter.VotingManaCurrent = objects.UseMana(voter.VotingManaCurrent, remainder, 0)
			} else {
				voter.DownvoteManaCurrent = objects.UseMana(voter.DownvoteManaCurrent, usedPower, 0)
			}
		} else {
			voter.VotingManaCurrent = objects.UseMana(voter.VotingManaCurrent, usedPower, 0)
		}
	} else {
		voter.VotingManaCurrent = currentPower - usedPower
		voter.VotingManaLastUpdate = ctx.HeadBlockTime
	}
	if vestTraceAccount(string(v.Voter)) {
		ghiveDiff("GHIVEDIFF VOTE-MANA block=%d voter=%s author=%s permlink=%s weight=%d hf20=%v current=%d used=%d rshares=%d mana_after=%d last_update=%d\n",
			ctx.BlockNum, v.Voter, v.Author, v.Permlink, v.Weight,
			ctx.Hardforks.Has(hardfork.HF20), currentPower, usedPower, rshares,
			voter.VotingManaCurrent, voter.VotingManaLastUpdate)
	}

	// Compute curation weight for this vote — the W(R_N) - W(R_N-1)
	// increment on the comment's running positive-rshares total.
	// Mirrors hive_evaluator_social.cpp:492-554. Only positive
	// (upvote) rshares contribute to curation weight; downvotes and
	// recasts get zero curation weight.
	//
	// hived distinguishes two values:
	//   max_vote_weight = bounded-curve delta (pre-discount)
	//   vote_weight     = max_vote_weight × min(elapsed, window) / window
	// The CommentVote stores `vote_weight` (post-discount); the
	// comment's TotalVoteWeight accumulates `max_vote_weight`
	// (pre-discount). At cashout this gap leaves a curation
	// "remainder" which forwards to the author pre-HF20.
	// HIVE_HARDFORK_0_6_REVERSE_AUCTION_TIME = HF6_TIME - 6d = 1466776800.
	// Comments created before this use the rshares³/abs_rshares² weight
	// (hive_evaluator_social.cpp:502-516); after, the bounded W(R) curve.
	const hf6ReverseAuctionTimeCubed uint32 = 1466776800
	var voteCurationWeight, maxVoteWeight uint64
	// hive_evaluator_social.cpp:515-517: curation_reward_eligible is also
	// gated on !was_paid — votes cast on a comment that has already paid
	// out (in its second-cashout window) accumulate zero curation weight,
	// so the curator_pool stays unclaimed and folds back to the author.
	curationEligible := rshares > 0 && c.AllowCurationRewards && isNew && !c.WasPaid
	if curationEligible {
		oldVoteRshares := c.VoteRshares
		newVoteRshares := oldVoteRshares + uint64(rshares)
		if c.Created < hf6ReverseAuctionTimeCubed {
			// rshares > 0 already gated above (curationEligible).
			rsharesU := uint64(rshares)
			postAbs := uint64(c.AbsRshares + absRshares)
			if !ctx.Hardforks.Has(hardfork.HF1) {
				// Pre-HF1 (block < 328750): cube = (rshares*1M)^3 and
				// total2 = (postAbs*1M)^2, so the formula reduces to
				// rshares^3 * 1_000_000 / postAbs^2. Uses math128 / U256-
				// via-two-step to stay alloc-free in a path that fires
				// only on votes targeting comments created before block
				// 328k. Math: result = (r3 * 1M) / t2, where r3=rshares^3
				// (U192) and t2=postAbs^2 (U128). To keep within U192:
				// compute q1=r3/t2 (U64) and rem=r3 mod t2 (U128), then
				// add (q1*1M)+(rem*1M/t2).
				r2 := math128.Mul64Wide(rsharesU, rsharesU)
				r3 := math128.Mul64x128(rsharesU, r2)
				t2 := math128.Mul64Wide(postAbs, postAbs)
				if t2.IsZero() {
					voteCurationWeight = 0
				} else {
					q1, rem := r3.QuoRem(t2)
					// q1 * 1M
					qHi1, qLo1 := bits.Mul64(q1, 1_000_000)
					if qHi1 != 0 {
						voteCurationWeight = ^uint64(0)
					} else {
						// (rem * 1M) / t2: rem<t2≤2^128, *1M fits in U192.
						r1m := math128.Mul64x128(1_000_000, rem)
						q2, _ := r1m.QuoRem(t2)
						lo, c := bits.Add64(qLo1, q2, 0)
						if c != 0 {
							voteCurationWeight = ^uint64(0)
						} else {
							voteCurationWeight = lo
						}
					}
				}
			} else {
				// HF1+ pre-HF6: cube = rshares^3, total2 = postAbs^2,
				// result = cube / total2. U192 / U128 → uint64 saturated.
				r2 := math128.Mul64Wide(rsharesU, rsharesU)
				r3 := math128.Mul64x128(rsharesU, r2)
				t2 := math128.Mul64Wide(postAbs, postAbs)
				if t2.IsZero() {
					voteCurationWeight = 0
				} else {
					q, _ := r3.QuoRem(t2)
					voteCurationWeight = q
				}
			}
		} else {
			oldW := curationWeightCurve(ctx, c, oldVoteRshares)
			newW := curationWeightCurve(ctx, c, newVoteRshares)
			if newW > oldW {
				voteCurationWeight = newW - oldW
			}
		}
		maxVoteWeight = voteCurationWeight
		// Reverse-auction discount: hived gates this on _now >
		// HIVE_HARDFORK_0_6_REVERSE_AUCTION_TIME (1466776800 = HF6_TIME-6d,
		// hive_evaluator_social.cpp:544), NOT on the HF6 block flag —
		// the discount engages 6 days before the HF6 block actually
		// activates so consensus matches the chain that ran it then.
		// dgpo.ReverseAuctionSeconds defaults to
		// HIVE_REVERSE_AUCTION_WINDOW_SECONDS_HF6 (30 min) at chain start
		// (chain/include/.../global_property_object.hpp:139); we mirror
		// with a time-based fallback so the discount fires pre-HF6-block.
		const hf6ReverseAuctionTime uint32 = 1466776800
		dgpoRef, _ := objects.GetDGPORef(ctx.DB)
		reverseAuction := uint32(0)
		if dgpoRef != nil && dgpoRef.ReverseAuctionSeconds > 0 {
			reverseAuction = dgpoRef.ReverseAuctionSeconds
		} else if !ctx.Hardforks.Has(hardfork.HF25) && ctx.HeadBlockTime > hf6ReverseAuctionTime {
			// Pre-HF6-block fallback ONLY: once HF25 explicitly sets
			// dgpo.reverse_auction_seconds = 0, zero means ZERO (hived's
			// `_seconds < get_reverse_auction_seconds()` is never true) —
			// resurrecting the 30-min ramp here penalized every early
			// curator post-HF25 (anadolu reward_vests 6.5% short at
			// 55,236,769 = HF25+1002). Same bug class as HBDPrintRate
			// 0-means-unset.
			reverseAuction = 30 * 60 // HIVE_REVERSE_AUCTION_WINDOW_SECONDS_HF6
		}
		if reverseAuction > 0 && voteCurationWeight > 0 && ctx.HeadBlockTime > hf6ReverseAuctionTime {
			elapsed := uint32(0)
			if ctx.HeadBlockTime > c.Created {
				elapsed = ctx.HeadBlockTime - c.Created
			}
			if elapsed > reverseAuction {
				elapsed = reverseAuction
			}
			// (voteCurationWeight * elapsed) / reverseAuction.
			// Numerator fits in 96 bits (uint64 * uint32); use uint128
			// math to avoid big.Int allocs on every vote.
			hi, lo := bits.Mul64(voteCurationWeight, uint64(elapsed))
			q, _ := bits.Div64(hi, lo, uint64(reverseAuction))
			voteCurationWeight = q
		}
		// HF25+ three-phase discount past the reverse-auction window.
		// Mirrors C++ hive_evaluator_social.cpp:810-826: the discount
		// only fires when `dgpo.get_early_voting_seconds() > 0`, which
		// hived sets ONLY at HF25 activation (database_hardfork.cpp:
		// 480-481). HF21 set reverse_auction_seconds = 5min but left
		// early_voting_seconds / mid_voting_seconds at struct default
		// 0, so the entire HF21→HF25 window relies on the linear
		// reverse-auction ramp ABOVE without the additional /=2 / /=8
		// phases. Keying the gate off HF25 (not HF21) avoids
		// double-discounting curation in the HF21–HF25 window (blocks
		// ~33M–62M).
		//
		// Constants HIVE_EARLY_VOTING_SECONDS_HF25 = 24h,
		// HIVE_MID_VOTING_SECONDS_HF25 = 48h (config.hpp:186-187).
		if ctx.Hardforks.Has(hardfork.HF25) && voteCurationWeight > 0 {
			const earlyVotingSeconds uint32 = 24 * 3600
			const midVotingSeconds uint32 = 48 * 3600
			elapsed := uint32(0)
			if ctx.HeadBlockTime > c.Created {
				elapsed = ctx.HeadBlockTime - c.Created
			}
			// Hived gates on `_seconds >= early_voting_seconds`. The
			// elapsed >= mid path subsumes elapsed >= early so the
			// branching maps to the same set partition.
			//
			// C++ sets max_vote_weight = vote_weight AFTER the phase
			// division (hive_evaluator_social.cpp:812-825) — unlike the
			// reverse-auction branch, the DISCOUNTED weight is what
			// accumulates into total_vote_weight. Leaving maxVoteWeight
			// undiscounted inflated TotalVoteWeight and diluted every
			// curator's weight/total share on posts with 24h+ votes.
			if elapsed >= earlyVotingSeconds+midVotingSeconds {
				voteCurationWeight /= 8
				maxVoteWeight = voteCurationWeight
			} else if elapsed >= earlyVotingSeconds {
				voteCurationWeight /= 2
				maxVoteWeight = voteCurationWeight
			}
		}
		c.VoteRshares = newVoteRshares
	}
	// hived's accumulate_vote_rshares (hive_evaluator_social.cpp:485)
	// always adds (rshares > 0 ? rshares : 0) to vote_rshares for new
	// votes — UNCONDITIONALLY, regardless of curation eligibility,
	// allows_curation, or was_paid. Our curationEligible block above
	// gates VoteRshares update on those flags; for new positive votes
	// where curationEligible is FALSE (e.g., post-cashout votes pre-HF12,
	// !AllowCurationRewards), we still need to bump VoteRshares so
	// future curation_weight calcs match C++.
	if !curationEligible && isNew && rshares > 0 {
		c.VoteRshares += uint64(rshares)
	}

	// hived's adjust_rshares2: the global total_reward_shares2 must move
	// by curve(new_net_rshares) - curve(old_net_rshares) on every cast/
	// recast so the comment's slice of the reward fund reflects the
	// updated total at cashout.
	oldNetRshares := c.NetRshares
	voteDebug := voteDebugAuthor != "" && string(v.Author) == voteDebugAuthor &&
		(voteDebugPermlink == "" || string(v.Permlink) == voteDebugPermlink)
	if isNew {
		objects.PutCommentVote(ctx.DB, &objects.CommentVote{
			Voter: v.Voter,
			// Share the comment's resident Author/Permlink backing strings
			// (identical values — c was looked up by v.Author/v.Permlink) so
			// the stored vote doesn't retain its own permlink copy. Permlinks
			// (often >16B, not interned) are the #1 resident object source
			// (~50% of live objects); votes are the biggest duplicator.
			Author:         c.Author,
			Permlink:       c.Permlink,
			Weight:         v.Weight,
			NumChanges:     0,
			Rshares:        rshares,
			LastUpdate:     ctx.HeadBlockTime,
			CurationWeight: voteCurationWeight,
		})
		c.NetRshares += rshares
		c.VoteCount++
		c.TotalVoteWeight += maxVoteWeight
		if voteDebug {
			ghiveDiff("GHIVEDIFF VOTE block=%d t=%d voter=%s author=%s permlink=%s weight=%d rshares=%d abs_rshares=%d max_vote_weight=%d vote_weight=%d total_vote_weight_after=%d vote_rshares_after=%d eligible=%t\n",
				ctx.BlockNum, ctx.HeadBlockTime, v.Voter, v.Author, v.Permlink,
				v.Weight, rshares, absRshares, maxVoteWeight, voteCurationWeight,
				c.TotalVoteWeight, c.VoteRshares, curationEligible)
		}
	} else {
		// Recast: subtract the previous vote's curation_weight from the
		// running total. C++ `c.accumulate_vote_weight(-itr->get_weight())`
		// (hive_evaluator_social.cpp:600) which is `total += -prev` — a
		// uint64 += int64 (negative). When total < prev, this WRAPS
		// to ~2^64. Mirror the wraparound (no guard) — the defensive
		// `>= prev` guard we had skipped the subtraction in that case,
		// drifting state from C++ over millions of recasts.
		c.TotalVoteWeight -= prev.CurationWeight
		c.NetRshares += rshares - prev.Rshares
		// HF26+ ONLY: update VoteRshares (positive-only accumulator) on
		// edits. The HF20 evaluator's pre-HF26 edit branch is
		// `c.accumulate_vote_rshares(rshares - previous_rshares, 0)`
		// (hive_evaluator_social.cpp:832-835) — the net moves but the
		// positive accumulator gets ZERO; only HF26's
		// NO_VOTE_EDIT_PENALTY routes edits through the fresh-vote
		// accumulation. Gating this at HF20 nudged vote_rshares on every
		// post-HF20 recast (first hit: steemsportsfund's 100%→85% recast
		// at block 26,305,775 on steemsports/dodgers-and-padres-..., which
		// shifted every later curator's sqrt-curve weight on the comment).
		//
		// HF26's NO_VOTE_EDIT_PENALTY ALSO gives the edited vote a FRESH
		// curation weight (hive_evaluator_social.cpp:757-830): the edit
		// routes through the same W(R_new)−W(R_old) computation as a new
		// vote, with the old curve base shifted down by the prior vote's
		// positive rshares (`old_vote_rshares -= previous_positive_rshares`),
		// then the HF25 early/mid-phase discounts. total_vote_weight
		// accumulates `max_vote_weight − previous_vote_weight` (the
		// subtraction above), and the stored vote gets the discounted
		// weight. Leaving edits at weight 0 post-HF26 starved every
		// re-voter's curation share (ace108 reward_vests 0.33% short at
		// 68,735,093, ~58K blocks after HF26).
		recastCurationWeight := uint64(0)
		if ctx.Hardforks.Has(hardfork.HF26) {
			posNew := int64(0)
			if rshares > 0 {
				posNew = rshares
			}
			posPrev := int64(0)
			if prev.Rshares > 0 {
				posPrev = prev.Rshares
			}
			// Curve base BEFORE this edit's accumulation, minus the prior
			// vote's positive contribution.
			oldBase := c.VoteRshares - uint64(posPrev)
			delta := posNew - posPrev
			if delta >= 0 {
				c.VoteRshares += uint64(delta)
			} else {
				c.VoteRshares -= uint64(-delta)
			}
			if rshares > 0 && c.AllowCurationRewards && !c.WasPaid {
				oldW := curationWeightCurve(ctx, c, oldBase)
				newW := curationWeightCurve(ctx, c, c.VoteRshares)
				var w uint64
				if newW > oldW {
					w = newW - oldW
				}
				maxW := w
				// HF26 implies HF25: reverse_auction_seconds is 0 (that
				// branch never fires), only the early/mid phase discounts
				// apply — same shape as the fresh-vote path.
				if w > 0 {
					const earlyVotingSeconds uint32 = 24 * 3600
					const midVotingSeconds uint32 = 48 * 3600
					elapsed := uint32(0)
					if ctx.HeadBlockTime > c.Created {
						elapsed = ctx.HeadBlockTime - c.Created
					}
					if elapsed >= earlyVotingSeconds+midVotingSeconds {
						w /= 8
						maxW = w
					} else if elapsed >= earlyVotingSeconds {
						w /= 2
						maxW = w
					}
				}
				recastCurationWeight = w
				c.TotalVoteWeight += maxW
			}
		}
		// Build a fresh CommentVote rather than mutating *prev — prev
		// aliases the in-place store slot under GetCommentVoteRef, and
		// mutating it before PutCommentVote would scramble the undo
		// snap (which captures post-mutation state in the UndoEnabled
		// path).
		recast := objects.CommentVote{
			Voter:          v.Voter,
			Author:         c.Author, // share comment's backing (see isNew path)
			Permlink:       c.Permlink,
			Weight:         v.Weight,
			NumChanges:     prev.NumChanges + 1,
			Rshares:        rshares,
			LastUpdate:     ctx.HeadBlockTime,
			CurationWeight: recastCurationWeight, // 0 pre-HF26 (edit penalty)
		}
		objects.PutCommentVote(ctx.DB, &recast)
		if voteDebug {
			ghiveDiff("GHIVEDIFF VOTE-RECAST block=%d t=%d voter=%s author=%s permlink=%s weight=%d rshares=%d prev_rshares=%d prev_curation_weight=%d total_vote_weight_after=%d\n",
				ctx.BlockNum, ctx.HeadBlockTime, v.Voter, v.Author, v.Permlink,
				v.Weight, rshares, recast.Rshares, recast.CurationWeight, c.TotalVoteWeight)
		}
	}
	// Track AbsRshares for the beneficiary "no votes yet" gate.
	if absRshares > 0 {
		c.AbsRshares += absRshares
	}
	// was_voted_on (hived on_vote real_vote, hive_evaluator_social.cpp:464/
	// 752): set on every vote pre-HF20 and at HF26+, else only when
	// abs_rshares != 0 (HF20..HF26 dust-vote exclusion). Sticky. Drives the
	// comment_options "option tightening requires no votes" gate.
	if absRshares != 0 || !ctx.Hardforks.Has(hardfork.HF20) || ctx.Hardforks.Has(hardfork.HF26) {
		c.WasVotedOn = true
	}
	c.LastUpdate = ctx.HeadBlockTime

	// Pre-HF17 (HARDFORK_0_17__769) cashout-time weighted average.
	// Mirrors hive_evaluator_social.cpp:403-449. Every cast adjusts the
	// ROOT post's children_abs_rshares + cashout_time + max_cashout_time
	// (max_cashout_time gets seeded on first vote to now+14d).
	//
	// formula:
	//   new_cashout_time = (cur_time × old_root_abs + new_time × abs) / new_root_abs
	//   new_time = HF12 + !HF13_257 ? now + 12h : now + 24h
	//   capped at root.max_cashout_time
	//
	// Pre-HF12: new_time = now + 24h (since HF12 not active).
	// Frozen-at-creation comments (cashout_time == 0) get pulled to a
	// real time on the first vote. The schedule entry is removed at
	// the old cashout_time and re-inserted at the new one.
	if !ctx.Hardforks.Has(hardfork.HF17) {
		// Locate the root: replies follow RootAuthor/RootPermlink, root
		// posts are their own root.
		rootAuthor := c.RootAuthor
		rootPermlink := c.RootPermlink
		rootIsSelf := rootAuthor == c.Author && rootPermlink == c.Permlink
		if rootAuthor == "" {
			rootAuthor = c.Author
			rootPermlink = c.Permlink
			rootIsSelf = true
		}
		var root objects.Comment
		if rootIsSelf {
			root = *c
		} else {
			r, rerr := objects.GetComment(ctx.DB, rootAuthor, rootPermlink)
			if rerr != nil {
				goto rootDone
			}
			root = r
		}
		oldRootCashout := root.CashoutTime
		oldRootAbsRshares := root.ChildrenAbsRshares
		newRootAbsRshares := oldRootAbsRshares + uint64(absRshares)

		// max_cashout_time seed (now + HIVE_MAX_CASHOUT_WINDOW = 14d).
		const hiveMaxCashoutWindowSeconds uint32 = 14 * 24 * 60 * 60
		if root.MaxCashoutTime == 0 {
			root.MaxCashoutTime = ctx.HeadBlockTime + hiveMaxCashoutWindowSeconds
		}

		// HF12 + WasPaid root: post-cashout votes pin the next cashout to
		// last_payout + HIVE_SECOND_CASHOUT_WINDOW (30d), bypassing the
		// weighted-average pull. Mirrors hive_evaluator_social.cpp:418-421.
		// Without this branch, post-payout votes pull the second-cashout
		// time via weighted-avg which drifts vs C++. Together with the
		// vote-gate fix (root-payout-time check) this handles the
		// post-cashout vote semantics correctly.
		if ctx.Hardforks.Has(hardfork.HF12) && root.WasPaid && root.LastPayout > 0 {
			const hiveSecondCashoutWindow uint32 = 60 * 60 * 24 * 30
			newCashout := root.LastPayout + hiveSecondCashoutWindow
			root.CashoutTime = newCashout
			// PutCommentCashoutSchedule handles the by-time rebind when
			// commentID already exists — no separate Delete needed.
			if newCashout != oldRootCashout {
				objects.PutCommentCashoutSchedule(ctx.DB, newCashout, root.ID, root.Author, root.Permlink)
			}
		} else if ctx.Hardforks.Has(hardfork.HF14) && absRshares == 0 {
			// HF14 weight==0 cleanup: still clamp root cashout to max.
			// Mirrors hive_evaluator_social.cpp:435-437,449,461 which
			// always sets cashout_time = min(cur, max_cashout_time)
			// regardless of the abs_rshares==0 short-circuit. Effect
			// kicks in only when oldRootCashout > MaxCashoutTime, rare.
			if root.MaxCashoutTime != 0 && oldRootCashout > root.MaxCashoutTime {
				newCashout := root.MaxCashoutTime
				if oldRootCashout != newCashout {
					root.CashoutTime = newCashout
					objects.PutCommentCashoutSchedule(ctx.DB, newCashout, root.ID, root.Author, root.Permlink)
				}
			}
		} else {
			var newCashoutTimeSec uint32
			if ctx.Hardforks.Has(hardfork.HF12) && !ctx.Hardforks.Has(hardfork.HF13) {
				newCashoutTimeSec = ctx.HeadBlockTime + cashoutWindowSecondsPreHF17 // 12h
			} else {
				newCashoutTimeSec = ctx.HeadBlockTime + cashoutWindowSecondsPreHF12 // 24h
			}
			// Weighted average. cur_cashout_time is "discussion payout
			// time" (root's CashoutTime, capped at MaxCashoutTime).
			curCashout := uint64(oldRootCashout)
			if curCashout == 0 || (root.MaxCashoutTime != 0 && curCashout > uint64(root.MaxCashoutTime)) {
				curCashout = uint64(root.MaxCashoutTime)
			}
			var avgCashoutSec uint64
			if newRootAbsRshares > 0 {
				// num = curCashout * oldRootAbsRshares + newCashoutTimeSec * absRshares.
				// Each product fits in U128 (uint32×uint64); their sum fits
				// in U128 with one carry bit headroom. Divide by uint64
				// (newRootAbsRshares) to land in uint64 range — matches the
				// prior math/big.IsUint64() check.
				t1 := math128.Mul64Wide(curCashout, oldRootAbsRshares)
				t2 := math128.Mul64Wide(uint64(newCashoutTimeSec), uint64(absRshares))
				numU := t1.Add(t2)
				if numU.Hi < newRootAbsRshares {
					q, _ := bits.Div64(numU.Hi, numU.Lo, newRootAbsRshares)
					avgCashoutSec = q
				}
				// numU.Hi >= newRootAbsRshares ⇒ true quotient > uint64 max,
				// leave avgCashoutSec=0 (matches old !IsUint64 branch).
			} else {
				avgCashoutSec = uint64(newCashoutTimeSec)
			}
			if avgCashoutSec > uint64(root.MaxCashoutTime) {
				avgCashoutSec = uint64(root.MaxCashoutTime)
			}
			newCashout := uint32(avgCashoutSec)
			root.CashoutTime = newCashout
			// PutCommentCashoutSchedule rebinds the by-time entry when
			// commentID already exists (it tracks the prev cashoutTime
			// in the value record). No separate Delete needed.
			if newCashout != 0 && newCashout != oldRootCashout {
				objects.PutCommentCashoutSchedule(ctx.DB, newCashout, root.ID, root.Author, root.Permlink)
			}
		}
		root.ChildrenAbsRshares = newRootAbsRshares
		if rootIsSelf {
			*c = root // mirror updates back into c so PutComment below persists them
		} else {
			objects.PutComment(ctx.DB, &root)
		}
	}
rootDone:
	// Under !UndoEnabled, c aliases the slot — PutComment is a no-op
	// self-copy (elided by CommentStore.Put when cur == c). Under
	// UndoEnabled, c is a local value and PutComment captures the
	// snap before writing.
	objects.PutComment(ctx.DB, c)
	voter.LastVoteTime = ctx.HeadBlockTime
	if ctx.DB.Database().UndoEnabled {
		// Slot-swap path: commit the local copy back to the AccountStore
		// slot. Under !UndoEnabled, voter already aliases the slot and
		// the mutations are in-place — no PutAccount needed.
		objects.PutAccount(ctx.DB, voter)
	}
	var blockNumForAdjust uint32
	if dgpoRef, ok := objects.GetDGPORef(ctx.DB); ok {
		blockNumForAdjust = dgpoRef.HeadBlockNumber
	}
	adjustRewardShares2(ctx.DB, oldNetRshares, c.NetRshares, blockNumForAdjust)
	return nil
}

// curationWeightCurve evaluates the curve used for curation weights
// W(R). Pre-HF17 (HF1+): W(R) = uint64_max × R / (2s + R) using a
// uint128 multiply. Post-HF17 it's the reward_fund's curation_reward_curve
// (default bounded_curation, HF21 → linear at HF25).
//
// Mirrors hive_evaluator_social.cpp:519-539 + util/reward.cpp.
//
// All curves are uint64-bounded for realistic on-chain rshares (max
// magnitude ~2^50). The intermediate products may need uint128 (handled
// via math/bits.Mul64 + div128) but no result exceeds uint64. Native
// uint128 math replaces the previous math/big implementation (3+ heap
// allocs per call — 1.25 GB cum on a 4M-block run).
func curationWeightCurve(ctx *Context, c *objects.Comment, voteRshares uint64) uint64 {
	if voteRshares == 0 {
		return 0
	}

	// Pick the curve. HF17+ uses the reward_fund's curve (bounded_curation
	// by default, square_root pre-HF19_1052, linear post-HF21).
	if ctx.Hardforks.Has(hardfork.HF17) {
		fund, err := objects.GetRewardFund(ctx.DB, objects.PostRewardFundName)
		if err == nil {
			s := uint64(fund.ContentConstant)
			if fund.ContentConstant <= 0 {
				s = 2_000_000_000_000
			}
			// Mirror C++ hive_evaluator_social.cpp:522-523: comments
			// authored in the 7-day window before HF19 activation use
			// the square_root curve regardless of fund.curation_curve.
			// HIVE_HF_19_SQRT_PRE_CALC = HIVE_HARDFORK_0_19_ACTUAL_TIME -
			// HIVE_CASHOUT_WINDOW_SECONDS (0_19.hf:23-24). Critically the
			// ACTUAL_TIME is 1497970809 (15:00:09 UTC), NOT the nominal
			// HF19_TIME 1497970800 — they differ by 9 seconds. Using TIME
			// put the boundary 9s early, so comments created in
			// [1497366000,1497366009] got square_root in Go but
			// bounded_curation in hived → mixed-sign curation-weight drift
			// on comments cashing out at HF19 (block 12,988,978); richbaek's
			// curation share came out 4 sat-HIVE short, failing his
			// claim_reward_balance at 12,989,015.
			const hf19SqrtPreCalc uint32 = 1497970809 - 7*86400
			if !ctx.Hardforks.Has(hardfork.HF19) && c.Created > hf19SqrtPreCalc {
				// Mirror C++ util/reward.cpp:58 — sqrt uses
				// fc::uint128_approx_sqrt (mantissa approximation),
				// NOT exact Newton's. See math128/math128.go:154.
				return math128.U128{Lo: voteRshares}.ApproxSqrt()
			}
			switch fund.CurationRewardCurve {
			case objects.CurveLinear:
				return voteRshares
			case objects.CurveSquareRoot:
				return math128.U128{Lo: voteRshares}.ApproxSqrt()
			case objects.CurveConvergentLinear:
				// C++ reward.cpp:63 — ((r+s)² - s²) / (r + 4s).
				return curveConvergentLinear(voteRshares, s)
			case objects.CurveConvergentSquareRoot:
				// C++ reward.cpp:69 — rshares / approx_sqrt(rshares + 2s).
				den := math128.U128{Lo: voteRshares + 2*s}.ApproxSqrt()
				if den == 0 {
					return 0
				}
				return voteRshares / den
			default:
				// bounded_curation: (rshares << 64) / (2s + rshares).
				// Numerator is rshares << 64 (128-bit). Use bits.Div64.
				return divU128byU64(voteRshares /*hi*/, 0 /*lo*/, 2*s+voteRshares)
			}
		}
	}

	// Pre-HF17: W(R) = uint64_max × R / (2s + R), with 2s = 2 ×
	// HIVE_CONTENT_CONSTANT_HF0.
	const contentConstantHF0 uint64 = 2_000_000_000_000
	hi, lo := bits.Mul64(^uint64(0), voteRshares)
	den := 2*contentConstantHF0 + voteRshares
	q, _ := bits.Div64(hi, lo, den)
	return q
}

// curveConvergentLinear computes ((r+s)² - s²) / (r + 4s) using native
// uint128 arithmetic. Intermediate (r+s)² can be up to 2^128 for the
// extreme rshares range we care about (r ≤ 2^50, s ≈ 2^41 → r+s ≤ 2^51,
// (r+s)² ≤ 2^102 — comfortably within uint128). Result is uint64.
func curveConvergentLinear(r, s uint64) uint64 {
	rps := r + s
	// hi:lo = (r+s)² - s²
	hi, lo := bits.Mul64(rps, rps)
	sh, sl := bits.Mul64(s, s)
	// Subtract sh:sl from hi:lo.
	lo, borrow := bits.Sub64(lo, sl, 0)
	hi, _ = bits.Sub64(hi, sh, borrow)
	den := r + 4*s
	if den == 0 {
		return 0
	}
	q, _ := bits.Div64(hi, lo, den)
	return q
}

// divU128byU64 returns (hi:lo) / d. Returns 0 when d == 0 to mirror
// the pre-existing math/big path that returned an empty big.Int on
// divide-by-zero (caller handles a zero result).
func divU128byU64(hi, lo, d uint64) uint64 {
	if d == 0 || hi >= d {
		// Overflow or div-by-zero. The old math/big path silently
		// produced an unbounded value; we cap at uint64_max for safety.
		// In practice this never fires because hi < d holds for all
		// well-formed curve inputs.
		return ^uint64(0)
	}
	q, _ := bits.Div64(hi, lo, d)
	return q
}

// isqrt64 returns the integer square root of n. Equivalent to
// big.Int{n}.Sqrt(). Uses Newton's method on uint64.
func isqrt64(n uint64) uint64 {
	if n == 0 {
		return 0
	}
	// Initial guess: 2^(ceil(log2(n))/2)
	x := uint64(1) << (uint(bits.Len64(n)+1) / 2)
	for {
		y := (x + n/x) / 2
		if y >= x {
			return x
		}
		x = y
	}
}

// adjustRewardShares2 mirrors hived's database::adjust_rshares2 for the
// pre-HF17 era only. Pre-HF17 every vote ADDed curve(new)-curve(old) to
// dgpo.total_reward_shares2 (via util/reward.cpp evaluate_reward_curve
// — quadratic only). HF17_774 introduces the reward_fund_object model
// where recent_claims is no longer a per-vote running sum: it decays
// each block and absorbs cashed-out comments' curves once per block in
// process_comment_cashout (database_comment.cpp:432-446). Hived gates
// the vote-time adjust with `if !has_hardfork(HF17_774)`
// (hive_evaluator_social.cpp:594-600); we mirror with the same gate.
// Without it our recent_claims drifts ~1%+ higher than hived's,
// shrinking every post-HF17 author/curator payout and surfacing as
// `claim_reward_balance: insufficient balance` at block 10,635,691
// (account "ocrdu").
func adjustRewardShares2(s *state.Session, oldNetRshares, newNetRshares int64, blockNum uint32) {
	if hardfork.SetForBlock(blockNum).Has(hardfork.HF17) {
		// Post-HF17: votes no longer touch recent_claims. The cashout
		// processor handles per-block decay + per-due-comment curve
		// absorb instead.
		return
	}
	objects.MutateDGPO(s, func(p *objects.DynamicGlobalProperty) {
		total := reward.Uint128FromBytes(p.TotalRewardShares2)
		oldCurve := reward.EvaluateRewardCurveQuadratic(oldNetRshares)
		newCurve := reward.EvaluateRewardCurveQuadratic(newNetRshares)
		total = total.Sub(oldCurve).Add(newCurve)
		p.TotalRewardShares2 = reward.Uint128ToBytes(total)
	})
}

// evaluateCurveForFund applies the reward_fund's author_reward_curve
// to rshares, using its content_constant. Mirrors the curve dispatch
// in libraries/chain/util/reward.cpp::evaluate_reward_curve.
//
// Returns a U128. All five curve variants produce values that fit in
// uint128 for the rshares range Hive ever sees (rshares ≤ 2^63, content
// constant ~2^41 → (r+s)² ≤ 2^128 with room to spare).
func evaluateCurveForFund(rshares int64, f *objects.RewardFund) math128.U128 {
	if rshares <= 0 {
		return math128.U128{}
	}
	r := uint64(rshares)
	s := uint64(f.ContentConstant)
	if f.ContentConstant <= 0 {
		s = 2_000_000_000_000
	}
	switch f.AuthorRewardCurve {
	case objects.CurveLinear:
		return math128.U128{Lo: r}
	case objects.CurveSquareRoot:
		// Mirror C++ util/reward.cpp:58 approx_sqrt (mantissa-MSB
		// approximation, biases UPWARD between perfect squares), NOT
		// exact Newton's. See math128/math128.go:154.
		return math128.U128{Lo: math128.U128{Lo: r}.ApproxSqrt()}
	case objects.CurveBoundedCurve:
		// (rshares << 64) / (2s + rshares). Numerator is 128-bit (Hi=r, Lo=0);
		// denominator fits in uint64 since 2s ≈ 2^42 and rshares ≤ 2^63.
		q, _ := bits.Div64(r /*hi*/, 0 /*lo*/, 2*s+r)
		return math128.U128{Lo: q}
	case objects.CurveConvergentLinear:
		// ((r+s)² - s²) / (r + 4s). Numerator fits in U128 (computed by
		// curveQuadraticGeneric). Denominator fits in uint64.
		num := curveQuadraticGeneric(r, s)
		den := r + 4*s
		if den == 0 {
			return math128.U128{}
		}
		// num / den where num is U128, den is uint64 — and quotient must
		// fit in U128 (it does: result is bounded by r).
		return u128DivU64(num, den)
	case objects.CurveConvergentSquareRoot:
		// rshares / approx_sqrt(rshares + 2s).
		den := math128.U128{Lo: r + 2*s}.ApproxSqrt()
		if den == 0 {
			return math128.U128{}
		}
		return math128.U128{Lo: r / den}
	default:
		// quadratic — same as reward.EvaluateRewardCurveQuadratic but with
		// fund-specific content constant.
		return curveQuadraticGeneric(r, s)
	}
}

// curveQuadraticGeneric computes (r+s)² - s² = r² + 2·r·s as a U128.
// Inputs r, s are uint64; r ≤ 2^63 (caller's responsibility).
func curveQuadraticGeneric(r, s uint64) math128.U128 {
	r2hi, r2lo := bits.Mul64(r, r)
	// 2·r·s — handle r > 2^62 by computing r·s then shifting left by 1.
	rshi, rslo := bits.Mul64(r, s)
	// shift left by 1
	rshi2 := (rshi << 1) | (rslo >> 63)
	rslo2 := rslo << 1
	// sum
	lo, c := bits.Add64(r2lo, rslo2, 0)
	hi, _ := bits.Add64(r2hi, rshi2, c)
	return math128.U128{Hi: hi, Lo: lo}
}

// u128DivU64 returns ⌊n / d⌋ as U128. d must be non-zero; quotient
// must fit in U128 (i.e., n.Hi < d * 2^64 ⇒ trivially true when d > 0).
func u128DivU64(n math128.U128, d uint64) math128.U128 {
	if n.Hi < d {
		q, _ := bits.Div64(n.Hi, n.Lo, d)
		return math128.U128{Lo: q}
	}
	// Quotient may exceed uint64. Two-step long division.
	qhi, rhi := bits.Div64(0, n.Hi, d)
	qlo, _ := bits.Div64(rhi, n.Lo, d)
	return math128.U128{Hi: qhi, Lo: qlo}
}

func init() {
	register(op.TagComment, commentEvaluator{})
	register(op.TagDeleteComment, deleteCommentEvaluator{})
	register(op.TagCommentOptions, commentOptionsEvaluator{})
	register(op.TagVote, voteEvaluator{})
}
