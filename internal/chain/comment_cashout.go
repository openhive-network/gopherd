package chain

import (
	"fmt"
	"math/bits"
	"os"
	"slices"
	"strings"

	"hived-go/internal/math128"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/reward"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// GHIVEDIFF env vars are read ONCE at process start so the per-block hot
// path doesn't pay map-lookup cost. Replay sets these at exec time and
// they don't change mid-process, so caching is safe.
var (
	envGhivediff         = os.Getenv("GHIVEDIFF")
	envGhivediffAll      = os.Getenv("GHIVEDIFF_ALL")
	envGhivediffAccounts = os.Getenv("GHIVEDIFF_ACCOUNTS")
	envGhivediffFeed     = os.Getenv("GHIVEDIFF_FEED")
	envGhivediffDumpFile = os.Getenv("GHIVEDIFF_DUMP_FILE")
	envGhivediffVsts     = os.Getenv("GHIVEDIFF_VSTS")
	envGhivediffPowbump  = os.Getenv("GHIVEDIFF_POWBUMP")
	envGhivediffReset    = os.Getenv("GHIVEDIFF_RESET")
)

func ghiveDiff(format string, args ...any) {
	if envGhivediff == "" {
		return
	}
	fmt.Fprintf(os.Stderr, format, args...)
}

// traceAccount returns true when the given account name should be traced.
// envGhivediff="" disables all trace; envGhivediff="1" or any non-list value
// traces a default set (hipster, proskynneo, abit, pfunk); a comma-separated
// list (e.g. envGhivediff="pfunk,abit,acidsun") restricts tracing to those.
func traceAccount(name string) bool {
	v := envGhivediff
	if v == "" {
		return false
	}
	if v == "1" || !strings.ContainsRune(v, ',') && len(v) <= 2 {
		// Default set.
		switch name {
		case "hipster", "proskynneo", "abit", "pfunk":
			return true
		}
		return false
	}
	for _, n := range strings.Split(v, ",") {
		if n == name {
			return true
		}
	}
	return false
}

// curationRewardsPercentPreHF8 — pre-HF8, curators get 50% of the
// claimed reward (matches get_curation_rewards_percent in C++).
// HF8 lowers it to 25%; post-HF17 the percentage comes from the
// reward_fund_object — we'll wire that in once that table lands.
//
// HIVE_1_PERCENT = 100, so 50 × 100 = 5000 basis points (50%).
const (
	curationPctPreHF8  int64 = 5000 // 50%
	curationPctPostHF8 int64 = 2500 // 25%
)

// curationPercentForBlock returns the curator-share basis points for the
// hardforks active at blockNum.
func curationPercentForBlock(blockNum uint32) int64 {
	hf := hardfork.SetForBlock(blockNum)
	if hf.Has(hardfork.HF8) {
		return curationPctPostHF8
	}
	return curationPctPreHF8
}

// processCommentCashouts pays out comments whose CashoutTime has elapsed.
// Mirrors libraries/chain/database_comment.cpp::process_comment_cashout,
// applied in time order against the per-block schedule index. blockNum
// drives the curation/author split (HF8 toggles 50/50 → 25/75).
//
// Hived gates the entire hook on HIVE_HARDFORK_0_7_FIRST_CASHOUT_TIME
// (block 2,889,019) — pre-HF7 the function returns early, letting the
// pre-HF7 backlog of 24h-due comments accumulate. At HF7 the queue
// drains in one cascade ("the first cashout will be 2+ months of
// rewards" — the C++ comment).
func processCommentCashouts(s *state.Session, blockTime, blockNum uint32, hooks *blockHookScratch) {
	if !hardfork.SetForBlock(blockNum).Has(hardfork.HF7) {
		return
	}
	store := objects.EnsureCommentCashoutScheduleStore(s)
	// Skip the entire hook when the earliest queued cashout is still
	// in the future — most blocks land here.
	if earliest, has := store.EarliestDue(); !has || earliest > blockTime {
		return
	}

	hooks.cashoutPending = hooks.cashoutPending[:0]
	ready := &hooks.cashoutPending

	// Iteration order matches C++'s composite_key<cashout_time,
	// comment_id>: smallest time first, then smallest id (insertion
	// order) within the same time.
	store.RangeDueBefore(blockTime, func(t uint32, id uint64, author auth.AccountName, permlink string) bool {
		*ready = append(*ready, blockHookCashoutPending{
			cashoutTime: t,
			commentID:   id,
			author:      string(author),
			permlink:    permlink,
		})
		return true
	})

	curationPct := curationPercentForBlock(blockNum)
	hf := hardfork.SetForBlock(blockNum)
	if hf.Has(hardfork.HF17) {
		// HF17+: pay each due comment individually (matches
		// database_comment.cpp:466-481).
		for _, p := range *ready {
			processOneCashout(s, blockTime, blockNum, p.cashoutTime, p.commentID, p.author, p.permlink, curationPct, hooks)
		}
		return
	}

	// Pre-HF17 thread-cashout grouping (database_comment.cpp:485-507):
	// for each ready entry, find the discussion ROOT and pay every
	// comment under that root_id together. Already-paid comments are
	// skipped via a per-call dedup set, so we never double-pay even
	// when multiple replies in the same thread land in `ready`.
	if hooks.paid == nil {
		hooks.paid = make(map[cashoutPaidKey]struct{}, 32)
	} else {
		for k := range hooks.paid {
			delete(hooks.paid, k)
		}
	}
	paid := hooks.paid
	for _, p := range *ready {
		// Read-only: only inspect Root* fields to find the discussion
		// root for this comment, then walk the by-root sibling list.
		c, ok := objects.GetCommentRef(s, p.author, p.permlink)
		if !ok {
			continue
		}
		rootAuthor := c.RootAuthor
		rootPermlink := c.RootPermlink
		if rootAuthor == "" {
			// Legacy row predating Root* — fall back to per-comment.
			rootAuthor = c.Author
			rootPermlink = c.Permlink
		}
		// Snapshot every comment under this root so we don't mutate the
		// by-root index while iterating. C++'s comment_cashout_ex_index
		// is composite_key(root_id, id) — within a root, comments cash
		// out in comment-id (creation) order. We must sort by ID to
		// match: the order in which create_vesting calls fire mutates
		// the global vesting share price between calls, so a different
		// order produces different per-curator/author VESTS amounts.
		hooks.siblings = hooks.siblings[:0]
		_ = objects.RangeCommentsByRoot(s, auth.AccountName(rootAuthor), rootPermlink,
			func(sib *objects.Comment) (bool, error) {
				hooks.siblings = append(hooks.siblings, cashoutSibling{
					author:      string(sib.Author),
					permlink:    sib.Permlink,
					cashoutTime: sib.CashoutTime,
					id:          sib.ID,
				})
				return true, nil
			})
		slices.SortFunc(hooks.siblings, func(a, b cashoutSibling) int {
			if a.id < b.id {
				return -1
			} else if a.id > b.id {
				return 1
			}
			return 0
		})
		for _, sib := range hooks.siblings {
			key := cashoutPaidKey{author: sib.author, permlink: sib.permlink}
			if _, dup := paid[key]; dup {
				continue
			}
			paid[key] = struct{}{}
			processOneCashout(s, blockTime, blockNum, sib.cashoutTime, sib.id, sib.author, sib.permlink, curationPct, hooks)
		}
	}
}

// processOneCashout pays out a single due comment, mirroring the no-HF17
// branch of cashout_comment_helper:
//
//	rshare_reward = total_reward_fund_hive × evaluate_reward_curve(net_rshares)
//	                                       / total_reward_shares2
//	curation_tokens = rshare_reward × curation_pct / 100%
//	author_tokens   = rshare_reward - curation_tokens
//
//	pay_curators distributes curation_tokens proportionally to each
//	  voter's rshares² weight; remainder folds back to author.
//	author_tokens splits per percent_hbd: hbd_hive (paid as liquid HIVE
//	  before the price feed) and vesting_hive (via create_vesting).
//
// adjust_rshares2 then subtracts the comment's curve value from
// total_reward_shares2 so the next comment's slice excludes this one.
func processOneCashout(s *state.Session, blockTime, blockNum, scheduledTime uint32, commentID uint64, author, permlink string, curationPct int64, hooks *blockHookScratch) {
	objects.DeleteCommentCashoutSchedule(s, scheduledTime, commentID)

	// processOneCashout runs in the per-block hooks (after the tx
	// loop's DiscardSavepoint), so UndoEnabled is always false here.
	// GetCommentRef gives a *Comment aliased to the CommentStore slot
	// — all field mutations below are in-place, and the trailing
	// PutComment becomes a no-op (skipped).
	c, ok := objects.GetCommentRef(s, author, permlink)
	if !ok {
		return
	}
	// Compute the post-cashout reschedule. Mirror of
	// libraries/chain/database_comment.cpp:343-353:
	//   - HF17+: cashout_time = max (frozen)
	//   - else if root post AND HF12 active AND !was_paid:
	//       cashout_time = head_block_time + HIVE_SECOND_CASHOUT_WINDOW (30d)
	//   - else: max (frozen)
	const hiveSecondCashoutWindow uint32 = 60 * 60 * 24 * 30
	wasPaidPre := c.WasPaid
	c.WasPaid = true
	c.LastPayout = blockTime
	rescheduleTime := uint32(0) // default: frozen
	isRoot := c.ParentAuthor == ""
	// blockNum already identifies the active hardfork set for this
	// cashout; the previous code did a GetDGPO + read of HeadBlockNumber
	// purely to call SetForBlock — redundant, since blockNum == the
	// cashout's containing block from processCommentCashouts.
	hfReschedule := hardfork.SetForBlock(blockNum)
	if !hfReschedule.Has(hardfork.HF17) && isRoot && hfReschedule.Has(hardfork.HF12) && !wasPaidPre {
		rescheduleTime = blockTime + hiveSecondCashoutWindow
	}
	c.CashoutTime = rescheduleTime
	// Mirror comment_cashout_object::on_payout (comment_object.hpp:213-219)
	// + comment_cashout_ex_object::on_payout (comment_object.hpp:343-349):
	// after a payout, zero the cashout-side accumulators (NetRshares,
	// VoteRshares, TotalVoteWeight) AND the ex-side accumulators
	// (AbsRshares, ChildrenAbsRshares, MaxCashoutTime). Without zeroing
	// the ex-side fields, a post-payout vote runs the weighted-avg
	// against stale pre-payout abs/children values, and Go's MaxCashoutTime
	// substitution (== 0 → MaxCashoutTime) lets stale max values pull the
	// avg into the past — the cashout fires immediately at the same block
	// and double-pays the author. C++ resets to maximum/0 here so the
	// post-payout vote behaves like the first vote on a fresh comment.
	//
	// Pre-HF19 also retains the NEGATIVE portion of net_rshares: hived
	// runs c.on_payout() (zero) and then c.accumulate_vote_rshares(
	// negative_rshares, 0) so downvote-driven negative net_rshares
	// "hangs around" for future post-payout upvotes (database_comment.cpp:
	// 384-405). HF19+ removes the cashout row entirely and skips this.
	hfCashout := hardfork.SetForBlock(blockNum)
	preservedNegative := int64(0)
	if !hfCashout.Has(hardfork.HF19) && c.NetRshares < 0 {
		preservedNegative = c.NetRshares
	}
	zero := func() {
		c.NetRshares = preservedNegative
		c.VoteRshares = 0
		c.TotalVoteWeight = 0
		c.AbsRshares = 0
		c.ChildrenAbsRshares = 0
		c.MaxCashoutTime = 0
	}
	defer func() {
		// c aliases the in-place slot — no PutComment needed.
		if rescheduleTime != 0 {
			objects.PutCommentCashoutSchedule(s, rescheduleTime, c.ID, c.Author, c.Permlink)
		}
	}()

	if !c.AllowCurationRewards || c.NetRshares <= 0 {
		// Frozen / no-payout comment: zero the on_payout fields AND
		// delete cv rows. C++ database_comment.cpp:417-424 deletes ALL
		// comment_vote rows at the end of cashout_helper, OUTSIDE the
		// `if (net_rshares > 0)` reward gate. Without this, NetRshares<=0
		// cashouts leave stale cv rows behind, so subsequent votes look
		// like recasts in our state but NEW votes in C++.
		zero()
		deleteCommentVotes(s, author, permlink)
		return
	}

	// Two pricing regimes:
	//   pre-HF17: payout = total_reward_fund_hive × curve(net_rshares) / total_reward_shares2
	//   post-HF17: payout = reward_fund.reward_balance × curve(net_rshares) / reward_fund.recent_claims
	// The reward_fund_object track is per-fund (we have only the "post"
	// fund) and uses curve_id-configurable evaluation. The pool values
	// come from different sources but the proportional payout shape is
	// identical.
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return
	}

	var (
		fundBalance int64
		shares2     = reward.Uint128FromBytes(dgpo.TotalRewardShares2)
		usingFund   bool
		fund        objects.RewardFund
	)
	if rf, ferr := objects.GetRewardFund(s, objects.PostRewardFundName); ferr == nil {
		usingFund = true
		fund = rf
		fundBalance = fund.RewardBalance
		shares2 = reward.Uint128FromBytes(fund.RecentClaims)
	} else {
		fundBalance = dgpo.TotalRewardFundHive
	}
	if fundBalance <= 0 {
		return
	}
	if shares2.IsZero() {
		return
	}

	// curve(net_rshares) — uses fund's content_constant + curve when the
	// reward_fund is in play; otherwise the legacy quadratic with
	// HIVE_CONTENT_CONSTANT_HF0.
	var commentCurve math128.U128
	if usingFund {
		commentCurve = evaluateRewardCurveU128(c.NetRshares, fund.AuthorRewardCurve, fund.ContentConstant)
	} else {
		commentCurve = reward.EvaluateRewardCurveQuadratic(c.NetRshares)
	}
	if commentCurve.IsZero() {
		return
	}

	// reward_weight haircut (HF12-HF17 ROOT posts only). Mirrors
	// libraries/chain/util/reward.cpp:18: `claim = (claim × reward_weight)
	// / HIVE_100_PERCENT`. RewardWeight==0 means the field hasn't been set
	// (legacy/replies/post-HF17) — treat as HIVE_100_PERCENT (no haircut).
	weightedCurve := commentCurve
	if c.RewardWeight != 0 && c.RewardWeight < 10000 {
		// (commentCurve × reward_weight) / 10000. Result fits in U128
		// (it's strictly ≤ commentCurve). Both prod and result are U128
		// for any rshares; use the U192/uint64 → U128 helper to handle
		// the wider intermediate.
		prod := math128.Mul64x128(uint64(c.RewardWeight), commentCurve)
		weightedCurve = u192DivU64ToU128(prod, 10000)
	}

	// payout = fundBalance × weightedCurve / shares2.
	// fundBalance fits in uint64, weightedCurve in U128 → product in U192.
	// shares2 is U128, quotient fits in uint64 (bounded by fundBalance).
	prodFB := math128.Mul64x128(uint64(fundBalance), weightedCurve)
	qClaim, _ := prodFB.QuoRem(shares2)
	if qClaim > uint64(1<<63-1) {
		qClaim = 1<<63 - 1
	}
	claimed := int64(qClaim)
	if traceAccount(author) || (envGhivediffAll != "" && blockNum >= 2888500 && blockNum <= 2910000) {
		ghiveDiff("GHIVEDIFF CASHOUT-CALC block=%d author=%s permlink=%s id=%d cashoutTime=%d rshares=%d fundBalance=%d shares2_hi=%d shares2_lo=%d claimed_pre_clip=%d\n",
			blockNum, author, permlink, c.ID, scheduledTime, c.NetRshares, fundBalance, shares2.Hi, shares2.Lo, claimed)
	}
	// MaxAcceptedPayout is denominated in HBD (HBD_asset in C++); to clip
	// against `claimed` (HIVE-sat), convert via the median feed.
	// Mirrors libraries/chain/util/reward.cpp:27-29 + database_comment.cpp:219:
	// `max_hive = to_hive(current_hive_price, max_hbd); payout = min(payout, max_hive)`.
	// One GetFeedHistory lookup, reused below for the min-payout gate.
	feedHistoryFetched, feedHistoryOK := objects.GetFeedHistory(s)
	feedReady := feedHistoryOK == nil && !priceIsZero(feedHistoryFetched.CurrentMedianHistory)
	maxClaim := c.MaxAcceptedPayout
	if feedReady {
		maxClaim = convertHBDToHive(c.MaxAcceptedPayout, feedHistoryFetched.CurrentMedianHistory)
		if maxClaim <= 0 {
			maxClaim = c.MaxAcceptedPayout
		}
	}
	if claimed > maxClaim {
		claimed = maxClaim
	}
	if claimed > fundBalance {
		claimed = fundBalance
	}
	// HIVE_MIN_PAYOUT_HBD = 20 satoshi (= 0.020 HBD). If the payout
	// expressed in HBD via the median feed is below this threshold,
	// hived zeroes the payout (libraries/chain/util/reward.cpp:24).
	if claimed > 0 && feedReady {
		if convertHiveToHBD(claimed, feedHistoryFetched.CurrentMedianHistory) < 20 {
			claimed = 0
		}
	}

	// adjust_rshares2 / recent_claims: drop this comment's curve from the
	// running total. Pre-HF17 the cashout helper explicitly calls
	// `adjust_rshares2(curve(net_rshares), 0)` at
	// database_comment.cpp:350-351; HF17+ the per-fund recent_claims
	// snapshot is decremented analogously.
	// Mirror C++ database.cpp:1673-1674 — uint128 wraps mod 2^128 on
	// transient underflow. Uint128ToBytes performs the mod, no clamp.
	shares2 = shares2.Sub(commentCurve)
	if usingFund {
		fund.RecentClaims = reward.Uint128ToBytes(shares2)
	} else {
		dgpo.TotalRewardShares2 = reward.Uint128ToBytes(shares2)
	}

	if claimed <= 0 {
		if usingFund {
			objects.PutRewardFund(s, &fund)
		} else {
			objects.PutDGPO(s, &dgpo)
		}
		// C++ cashout_comment_helper runs comment_cashout_ex::on_payout
		// (zeroing abs_rshares + children_abs_rshares + max_cashout_time)
		// unconditionally — see database_comment.cpp:391-396 — regardless
		// of whether reward was actually paid. We must do the same so a
		// dust-zeroed cashout doesn't leave stale ex-side accumulators
		// that the next vote then mis-uses in weighted-avg.
		zero()
		// C++ also deletes ALL comment_vote rows at the end of every
		// cashout (database_comment.cpp:417-424), regardless of whether
		// any reward was paid. Without deleting here, a dust-zero cashout
		// leaves the per-voter rows behind, so the NEXT cast by the same
		// voter looks like a "recast" (only the delta vs the prior rshares
		// is added to net_rshares) instead of a fresh +rshares — caused
		// pharesim/steemdollar-casino's 2nd cashout to be 250B rshares
		// short, dropping his hbd_payout from 487 to 460 sat HBD and
		// cascading into the 25-sat HBD insufficient-balance failure at
		// block 3,076,587 (limit_order_create vs bittrex transfer).
		deleteCommentVotes(s, author, permlink)
		return
	}

	// Decrement the source pool.
	if usingFund {
		fund.RewardBalance -= claimed
		objects.PutRewardFund(s, &fund)
	} else {
		dgpo.TotalRewardFundHive -= claimed
		objects.PutDGPO(s, &dgpo)
	}

	// HF17+ honors the reward_fund's per-curation share; pre-HF17 we
	// fall back to the legacy curationPct gate (50/50 pre-HF8, 25/75
	// post-HF8).
	if usingFund && fund.PercentCurationRewards > 0 {
		curationPct = int64(fund.PercentCurationRewards)
	}
	curatorPool := claimed * curationPct / 10000
	authorPool := claimed - curatorPool

	// Mirror C++ cashout_comment_helper order (database_comment.cpp:259-310):
	// CURATORS FIRST (so authors / beneficiaries see the post-curator
	// vesting share price), then beneficiaries, then author. Each
	// create_vesting2 call mutates total_vesting_fund_hive and
	// total_vesting_shares, so the per-recipient payout depends on the
	// order. Earlier Go paid beneficiaries before curators, which drifted
	// share prices for any post with both — dormant pre-HF17 (no
	// beneficiaries) but visible later.
	curatorRemainder := int64(0)
	if curatorPool > 0 {
		curatorRemainder = distributeCuratorPool(s, c.Author, c.Permlink, curatorPool, hooks)
	}
	hf := hardfork.SetForBlock(blockNum)
	if !hf.Has(hardfork.HF20) && curatorRemainder > 0 {
		authorPool += curatorRemainder
	}

	// Beneficiaries (HF17+): take their share OFF THE TOP of authorPool.
	// Each beneficiary's slice is paid as if the beneficiary were the
	// author — half via create_hbd, half via create_vesting. HF21+ has a
	// special branch when the beneficiary is the treasury: the entire
	// share is converted HIVE→HBD via current_median_history and credited
	// to the treasury account (no vesting leg). Mirrors
	// libraries/chain/database_comment.cpp:278-286.
	hfBene := hardfork.SetForBlock(blockNum)
	treasuryName := currentTreasuryName(s, hfBene)
	totalBeneficiary := int64(0)
	for _, b := range c.Beneficiaries {
		share := authorPool * int64(b.Weight) / 10000
		if share <= 0 {
			continue
		}
		bAcct, berr := objects.GetAccount(s, b.Account)
		if berr != nil {
			continue
		}
		if hfBene.Has(hardfork.HF21) && b.Account == treasuryName {
			// Treasury branch: convert HIVE→HBD at current median.
			fh, ferr := objects.GetFeedHistory(s)
			if ferr != nil || priceIsZero(fh.CurrentMedianHistory) {
				totalBeneficiary += share
				continue
			}
			hbdPayout := convertHiveToHBD(share, fh.CurrentMedianHistory)
			objects.AdjustHBDBalance(s, &bAcct, blockTime, hbdPayout)
			objects.PutAccount(s, &bAcct)
			// adjust_supply pair: HIVE -share burned, HBD +hbdPayout minted
			// (with VS recompute on the HBD branch). Mirrors C++
			// database_comment.cpp:283-285.
			objects.MutateDGPO(s, func(p *objects.DynamicGlobalProperty) {
				adjustSupplyHive(s, p, -share)
				adjustSupplyHbd(s, p, hbdPayout)
			})
			totalBeneficiary += share
			continue
		}
		hbdSlice := share * int64(c.PercentHbd) / 20000
		vestingSlice := share - hbdSlice
		if hbdSlice > 0 {
			payCreateHbd(s, &bAcct, blockTime, hbdSlice)
			objects.PutAccount(s, &bAcct)
		}
		if vestingSlice > 0 {
			_, _ = CreateVesting_via_chain(s, b.Account, vestingSlice, false)
		}
		totalBeneficiary += share
	}
	authorPool -= totalBeneficiary
	if authorPool < 0 {
		authorPool = 0
	}

	// Author payout split: half HBD-style (subject to print rate) +
	// half VESTS at the dynamic price.
	hbdHive := authorPool * int64(c.PercentHbd) / 20000
	vestingHive := authorPool - hbdHive
	if traceAccount(author) || (envGhivediffAll != "" && blockNum >= 2888500 && blockNum <= 2910000) {
		ghiveDiff("GHIVEDIFF CASHOUT-AUTHOR block=%d author=%s permlink=%s author_tokens=%d hbd_hive=%d vesting_hive=%d curator_remainder=%d\n",
			blockNum, author, permlink, authorPool, hbdHive, vestingHive, curatorRemainder)
	}
	if hbdHive > 0 {
		if a, gerr := objects.GetAccount(s, author); gerr == nil {
			payCreateHbd(s, &a, blockTime, hbdHive)
			objects.PutAccount(s, &a)
		}
	}
	if vestingHive > 0 {
		_, _ = CreateVesting_via_chain(s, author, vestingHive, false)
	}
	if traceAccount(author) {
		if a, gerr := objects.GetAccount(s, author); gerr == nil {
			ghiveDiff("GHIVEDIFF CASHOUT-POST-VEST block=%d author=%s permlink=%s vesting_shares=%d hive=%d hbd=%d\n",
				blockNum, author, permlink, a.VestingShares, a.Balance, a.HBDBalance)
		}
	}

	// Drop all CommentVote rows for this comment — hived's cashout
	// loop (libraries/chain/database_comment.cpp:366-373) removes them
	// to free memory and so any post-cashout re-vote starts fresh. Our
	// vote evaluator now also gates votes on CashoutTime==0 comments,
	// so this cleanup is mostly a memory hygiene measure.
	deleteCommentVotes(s, author, permlink)
	zero()
}

// deleteCommentVotes drops every vote row on (author, permlink). Uses
// the typed CommentVoteStore's bulk-delete which avoids the per-vote
// btree probe of the old secondary-index walk.
func deleteCommentVotes(s *state.Session, author, permlink string) {
	objects.DeleteAllCommentVotesForComment(s, auth.AccountName(author), permlink)
}

// evaluateRewardCurveU128 dispatches between the curve types selectable
// for a reward_fund_object. Mirrors libraries/chain/util/reward.cpp:
//
//	quadratic:   (rshares + s)² - s²
//	bounded:     (rshares << 64) / (2s + rshares)
//	linear:      rshares
//	square_root: floor(sqrt(rshares))
//	convergent_linear:        ((r+s)² - s²) / (r + 4s)
//	convergent_square_root:   r / sqrt(r + 2s)
//
// Native U128/U192 math replaces the previous math/big triple-alloc-
// per-call implementation that produced 1.5 GB of nat.make across a
// 4M-block run.
func evaluateRewardCurveU128(rshares int64, curve objects.CurveID, contentConstant int64) math128.U128 {
	if rshares <= 0 {
		return math128.U128{}
	}
	r := uint64(rshares)
	s := uint64(contentConstant)
	if contentConstant <= 0 {
		s = 2_000_000_000_000
	}
	switch curve {
	case objects.CurveLinear:
		return math128.U128{Lo: r}
	case objects.CurveSquareRoot:
		return math128.U128{Lo: chainISqrt64(r)}
	case objects.CurveBoundedCurve:
		// (rshares << 64) / (2s + rshares).
		q, _ := bits.Div64(r /*hi*/, 0 /*lo*/, 2*s+r)
		return math128.U128{Lo: q}
	case objects.CurveConvergentLinear:
		num := chainCurveQuadratic(r, s)
		den := r + 4*s
		if den == 0 {
			return math128.U128{}
		}
		return chainU128DivU64(num, den)
	case objects.CurveConvergentSquareRoot:
		den := chainISqrt64(r + 2*s)
		if den == 0 {
			return math128.U128{}
		}
		return math128.U128{Lo: r / den}
	default:
		return chainCurveQuadratic(r, s)
	}
}

// chainCurveQuadratic computes r² + 2·r·s as U128 (uint64 inputs).
func chainCurveQuadratic(r, s uint64) math128.U128 {
	r2hi, r2lo := bits.Mul64(r, r)
	rshi, rslo := bits.Mul64(r, s)
	rshi2 := (rshi << 1) | (rslo >> 63)
	rslo2 := rslo << 1
	lo, c := bits.Add64(r2lo, rslo2, 0)
	hi, _ := bits.Add64(r2hi, rshi2, c)
	return math128.U128{Hi: hi, Lo: lo}
}

// chainU128DivU64 returns U128 ⌊n / d⌋ when d is a uint64.
func chainU128DivU64(n math128.U128, d uint64) math128.U128 {
	if n.Hi < d {
		q, _ := bits.Div64(n.Hi, n.Lo, d)
		return math128.U128{Lo: q}
	}
	qhi, rhi := bits.Div64(0, n.Hi, d)
	qlo, _ := bits.Div64(rhi, n.Lo, d)
	return math128.U128{Hi: qhi, Lo: qlo}
}

// u192DivU64ToU128 returns U128 ⌊n/d⌋ when n is U192, d is uint64,
// and the true quotient is known to fit in U128. Long-divides word by
// word; saturates to ^U128 if the actual quotient overflows U128.
func u192DivU64ToU128(n math128.U192, d uint64) math128.U128 {
	if d == 0 {
		return math128.U128{}
	}
	// Step 1: q_top = n.Hi / d, r1 = n.Hi mod d. If q_top != 0 the
	// quotient overflows U128 — saturate.
	qtop := n.Hi / d
	r1 := n.Hi - qtop*d
	if qtop != 0 {
		return math128.U128{Hi: ^uint64(0), Lo: ^uint64(0)}
	}
	qmid, r2 := bits.Div64(r1, n.Mid, d)
	qlo, _ := bits.Div64(r2, n.Lo, d)
	return math128.U128{Hi: qmid, Lo: qlo}
}

// chainISqrt64 returns ⌊sqrt(n)⌋ via Newton's method on uint64.
func chainISqrt64(n uint64) uint64 {
	if n == 0 {
		return 0
	}
	x := uint64(1) << (uint(bits.Len64(n)+1) / 2)
	for {
		y := (x + n/x) / 2
		if y >= x {
			return x
		}
		x = y
	}
}

// payCreateHbd mirrors database::create_hbd in
// libraries/chain/database.cpp:949. The hive_amount (denominated in
// HIVE-satoshi) is split per the hbd_print_rate into a HBD portion (
// stored as the account's hbd_balance) and a HIVE portion (left as
// liquid balance). The HBD portion is computed by multiplying the
// HIVE-denominated piece by the median feed price.
//
// Without a median feed (early chain), all of hive_amount becomes
// liquid HIVE balance — same as hived's pre-feed branch.
//
// hbd_print_rate is not yet tracked; we use HIVE_100_PERCENT (10000),
// the post-genesis default, until the HF14 hard-cap logic lands.
func payCreateHbd(s *state.Session, a *objects.Account, blockTime uint32, hiveAmount int64) {
	if hiveAmount <= 0 {
		return
	}
	fh, err := objects.GetFeedHistory(s)
	if err != nil || priceIsZero(fh.CurrentMedianHistory) {
		// No feed → everything pays as liquid HIVE.
		a.Balance += hiveAmount
		return
	}
	// hbd_print_rate is recomputed each block by update_virtual_supply.
	// Pre-HF14 it stays at 10000 (100% — entire HIVE portion converts
	// to HBD). Post-HF14 it ramps down once HBD's share of virtual
	// supply rises past hbd_start_percent, eventually reaching 0
	// (0% to HBD, all liquid HIVE) when percent_hbd ≥ hbd_stop_percent.
	// We re-read DGPO here to pick up the latest value.
	//
	// IMPORTANT: 0 is a valid value (means 0% to HBD, all to HIVE).
	// The earlier `dgpo.HBDPrintRate > 0` guard was wrong — it treated
	// 0 as "uninitialized" and fell back to the genesis default of
	// 10000, sending 100% to HBD precisely when hived was sending 100%
	// to HIVE. Cumulative effect on cryptos through 6,198,475: -35,850
	// sat-HIVE balance / +6,681 sat-HBD balance.
	hbdPrintRate := int64(10000)
	if dgpoRef, ok := objects.GetDGPORef(s); ok {
		hbdPrintRate = int64(dgpoRef.HBDPrintRate)
	}
	toHbdHive := hiveAmount * hbdPrintRate / 10000
	toHive := hiveAmount - toHbdHive

	hbdAmount := convertHiveToHBD(toHbdHive, fh.CurrentMedianHistory)
	objects.AdjustHBDBalance(s, a, blockTime, hbdAmount)
	a.Balance += toHive

	// Mirror C++'s pair of adjust_supply calls in create_hbd
	// (database.cpp:979-980): adjust_supply(-to_hbd HIVE) bumps
	// current_supply and virtual_supply incrementally, then
	// adjust_supply(+hbd HBD) does a FULL recompute of virtual_supply
	// from current_supply + current_hbd_supply × median. Without the
	// HBD-side recompute, Go's intra-block virtual_supply lags C++'s by
	// `hbd × median` per cashout — visible to the next intra-block
	// reader (process_funds, pay_liquidity_reward) and (transitively)
	// to subsequent HBD-supply ratio calcs at next-block updateVirtualSupply.
	objects.MutateDGPO(s, func(p *objects.DynamicGlobalProperty) {
		adjustSupplyHive(s, p, -toHbdHive)
		adjustSupplyHbd(s, p, hbdAmount)
	})
}

// convertHiveToHBD applies a HBD/HIVE feed price to convert a HIVE
// amount into HBD. Mirror of convertHBDToHive but in the opposite
// direction: hbd = hive × base / quote (when feed orientation is
// HBD-base / HIVE-quote, the post-HF20 normalization).
func convertHiveToHBD(hiveAmount int64, p asset.Price) int64 {
	if p.Base.Amount <= 0 || p.Quote.Amount <= 0 {
		return 0
	}
	if p.Base.Symbol.Num == asset.HbdSymbol && p.Quote.Symbol.Num == asset.HiveSymbol {
		return mulDiv(hiveAmount, p.Base.Amount, p.Quote.Amount)
	}
	if p.Base.Symbol.Num == asset.HiveSymbol && p.Quote.Symbol.Num == asset.HbdSymbol {
		return mulDiv(hiveAmount, p.Quote.Amount, p.Base.Amount)
	}
	return 0
}

// distributeCuratorPool walks every CommentVote on (author, permlink)
// via the by-comment secondary index and credits each voter their
// proportional share of the pool, using the curation_weight that was
// computed and stored at vote time.
//
// Mirrors hived's database::pay_curators (database_comment.cpp:129):
//
//	for each voter:
//	    claim = pool × vote.curation_weight / comment.total_vote_weight
//	    credit `claim` HIVE to voter as VESTS via create_vesting
//
// If the comment has total_vote_weight == 0 (no upvotes), the entire
// pool stays unclaimed and folds back to the author (handled by the
// caller).
func distributeCuratorPool(s *state.Session, author, permlink string, pool int64, hooks *blockHookScratch) int64 {
	// Read-only access: only inspecting c.TotalVoteWeight to gate +
	// derive the denominator for the curator-share math.
	c, ok := objects.GetCommentRef(s, author, permlink)
	if !ok {
		return pool
	}
	if c.TotalVoteWeight == 0 || pool <= 0 {
		return pool
	}
	totalW := c.TotalVoteWeight

	// Reuse the per-block hook scratch list across cashouts; the
	// alternative was a heap-escaping `var pays []pay` per call. Each
	// cashout's distributeCuratorPool fires once and the scratch is
	// reset before use, so no aliasing between adjacent cashouts.
	hooks.curatorPays = hooks.curatorPays[:0]
	pays := &hooks.curatorPays

	_ = objects.RangeCommentVotesByComment(s, auth.AccountName(author), permlink,
		func(cv *objects.CommentVote) (bool, error) {
			if cv.CurationWeight == 0 {
				return true, nil
			}
			// share = pool × curation_weight / total_vote_weight.
			// Product up to uint64 × uint64 = uint128; divide by uint64.
			hi, lo := bits.Mul64(uint64(pool), cv.CurationWeight)
			if hi >= totalW {
				// Quotient overflows uint64 — skip.
				return true, nil
			}
			q, _ := bits.Div64(hi, lo, totalW)
			if q > uint64(1<<63-1) {
				return true, nil
			}
			s64 := int64(q)
			if s64 <= 0 {
				return true, nil
			}
			*pays = append(*pays, CuratorPay{Voter: string(cv.Voter), Weight: cv.CurationWeight, Share: s64})
			return true, nil
		})

	// hived's pay_curators iterates a std::set keyed by
	// (weight DESC, voter ASC) — see database_comment.cpp:131-140. Each
	// CreateVesting call mutates total_vesting_fund_hive/total_vesting_shares,
	// so the order curators are paid in subtly affects the share-price
	// each subsequent curator sees. Match the C++ order. slices.SortFunc
	// replaces sort.Slice to drop the reflectlite-swapper alloc on this
	// per-cashout hot path.
	slices.SortFunc(*pays, func(a, b CuratorPay) int {
		if a.Weight != b.Weight {
			if a.Weight > b.Weight {
				return -1
			}
			return 1
		}
		if a.Voter < b.Voter {
			return -1
		}
		if a.Voter > b.Voter {
			return 1
		}
		return 0
	})

	paid := int64(0)
	dgpoNow, _ := objects.GetDGPO(s)
	// HF17_659 (block 10,629,455 mainnet) flips curator vests to the
	// reward_balance — claimable later via claim_reward_balance —
	// instead of crediting vesting_shares directly. Mirrors C++ at
	// database_comment.cpp:188 `create_vesting2(... has_hardfork(HF17_659) ...)`.
	hf := hardfork.SetForBlock(dgpoNow.HeadBlockNumber)
	toReward := hf.Has(hardfork.HF17)
	for _, p := range *pays {
		if traceAccount(p.Voter) || traceAccount(author) {
			ghiveDiff("GHIVEDIFF CASHOUT-CURATOR block=%d author=%s permlink=%s voter=%s weight=%d total_weight=%d pool=%d claim=%d\n",
				dgpoNow.HeadBlockNumber, author, permlink, p.Voter, p.Weight, c.TotalVoteWeight, pool, p.Share)
		}
		_, _ = CreateVesting_via_chain(s, p.Voter, p.Share, toReward)
		paid += p.Share
	}
	if paid > pool {
		return 0
	}
	return pool - paid
}
