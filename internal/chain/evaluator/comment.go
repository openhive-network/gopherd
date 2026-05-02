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
		if dgpo, derr := objects.GetDGPO(ctx.DB); derr == nil {
			commentID = dgpo.NextCommentID
			dgpo.NextCommentID++
			objects.PutDGPO(ctx.DB, &dgpo)
		}
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
			RootAuthor:           rootAuthor,
			RootPermlink:         rootPermlink,
			ID:                   commentID,
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
	// error. Pre-HF24 it's a silent no-op. WasPaid + CashoutTime==0 is
	// our "fully paid out / cashout row removed" sentinel — distinct
	// from "frozen at creation, awaiting first vote" (CashoutTime==0
	// pre-HF12 / for replies, but WasPaid=false).
	if c.WasPaid && c.CashoutTime == 0 {
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
	// hive_evaluator_social.cpp:328: skip the vote when comment_cashout
	// is gone OR (HF12+ AND payout_time == max). PRE-HF12 the entire
	// gate is false (HF12 inactive), so votes on already-paid roots
	// fall through to the weighted-avg branch and re-pull cashout_time
	// — this is what re-schedules the comment for a fresh 24h cycle.
	// Skipping pre-HF12 here over-zeros children_abs_rshares cascades
	// at HF7 (block 2,889,020 et seq.) and pre-empts thousands of
	// follow-up votes whose mana drains C++ would otherwise have
	// captured.
	if ctx.Hardforks.Has(hardfork.HF12) && c.WasPaid && c.CashoutTime == 0 {
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
	// abs_rshares = effective_vesting_shares × used_power / HIVE_100_PERCENT
	// (hive_evaluator_social.cpp:364). C++ calls
	// get_effective_vesting_shares(false), which is
	// vesting_shares - delegated_vesting_shares + received_vesting_shares
	// (excludeWeeklyPowerDown=false ⇒ skip the next-withdrawal subtraction).
	// Voters with active delegations have effective != raw vesting; using
	// raw VestingShares here over-credits delegators and under-credits
	// delegatees, drifting shares2 over time.
	effectiveVesting := voter.VestingShares - voter.DelegatedVestingShares + voter.ReceivedVestingShares
	if effectiveVesting < 0 {
		effectiveVesting = 0
	}
	absRsharesBig := new(big.Int).Mul(big.NewInt(effectiveVesting), big.NewInt(usedPower))
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
		if lockout > 0 && c.CashoutTime > ctx.HeadBlockTime && c.CashoutTime-ctx.HeadBlockTime <= lockout {
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
	if rshares > 0 && c.AllowCurationRewards && err == state.ErrNotFound {
		oldVoteRshares := c.VoteRshares
		newVoteRshares := oldVoteRshares + uint64(rshares)
		if c.Created < hf6ReverseAuctionTimeCubed {
			rshares3 := new(big.Int).SetInt64(rshares)
			postAbs := c.AbsRshares + absRshares
			total2 := big.NewInt(postAbs)
			if !ctx.Hardforks.Has(hardfork.HF1) {
				m := big.NewInt(1_000_000)
				rshares3.Mul(rshares3, m)
				total2.Mul(total2, m)
			}
			cube := new(big.Int).Mul(rshares3, rshares3)
			cube.Mul(cube, rshares3)
			total2.Mul(total2, total2)
			if total2.Sign() > 0 {
				cube.Quo(cube, total2)
				if cube.IsUint64() {
					voteCurationWeight = cube.Uint64()
				} else {
					voteCurationWeight = ^uint64(0)
				}
			}
		} else {
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
		dgpo, _ := objects.GetDGPO(ctx.DB)
		reverseAuction := uint32(0)
		if dgpo.ReverseAuctionSeconds > 0 {
			reverseAuction = dgpo.ReverseAuctionSeconds
		} else if ctx.HeadBlockTime > hf6ReverseAuctionTime {
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
		c.TotalVoteWeight += maxVoteWeight
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
			root = c
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

		// Skip dust votes post-HF14 — they contribute zero rshares but
		// also don't pull cashout time (matches hive_evaluator_social.cpp:426).
		if !(ctx.Hardforks.Has(hardfork.HF14) && absRshares == 0) {
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
				num := new(big.Int).Mul(big.NewInt(int64(curCashout)), new(big.Int).SetUint64(oldRootAbsRshares))
				num.Add(num, new(big.Int).Mul(big.NewInt(int64(newCashoutTimeSec)), big.NewInt(absRshares)))
				num.Quo(num, new(big.Int).SetUint64(newRootAbsRshares))
				if num.IsUint64() {
					avgCashoutSec = num.Uint64()
				}
			} else {
				avgCashoutSec = uint64(newCashoutTimeSec)
			}
			if avgCashoutSec > uint64(root.MaxCashoutTime) {
				avgCashoutSec = uint64(root.MaxCashoutTime)
			}
			newCashout := uint32(avgCashoutSec)
			// Reschedule: drop old schedule entry, insert at new time.
			if oldRootCashout != 0 && oldRootCashout != newCashout {
				objects.DeleteCommentCashoutSchedule(ctx.DB, oldRootCashout, root.ID)
			}
			root.CashoutTime = newCashout
			if newCashout != 0 && newCashout != oldRootCashout {
				objects.PutCommentCashoutSchedule(ctx.DB, newCashout, root.ID, root.Author, root.Permlink)
			}
		}
		root.ChildrenAbsRshares = newRootAbsRshares
		if rootIsSelf {
			c = root // mirror updates back into c so PutComment below persists them
		} else {
			objects.PutComment(ctx.DB, &root)
		}
	}
rootDone:
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
