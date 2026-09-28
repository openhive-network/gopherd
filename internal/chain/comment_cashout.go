package chain

import (
	"fmt"
	"math/bits"
	"os"
	"slices"
	"strings"

	"gopherd/internal/math128"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/reward"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// GHIVEDIFF env vars are read ONCE at process start so the per-block hot
// path doesn't pay map-lookup cost. Replay sets these at exec time and
// they don't change mid-process, so caching is safe.
var (
	envGhivediff          = os.Getenv("GHIVEDIFF")
	envGhivediffAll       = os.Getenv("GHIVEDIFF_ALL")
	envGhivediffAccounts  = os.Getenv("GHIVEDIFF_ACCOUNTS")
	envGhivediffFeed      = os.Getenv("GHIVEDIFF_FEED")
	envGhivediffDumpFile  = os.Getenv("GHIVEDIFF_DUMP_FILE")
	envGhivediffVsts      = os.Getenv("GHIVEDIFF_VSTS")
	envGhivediffPowbump   = os.Getenv("GHIVEDIFF_POWBUMP")
	envGhivediffTimeshare = os.Getenv("GHIVEDIFF_TIMESHARE")
	envGhivediffGovExp    = os.Getenv("GHIVEDIFF_GOVEXP")
	envGhivediffFills     = os.Getenv("GHIVEDIFF_FILLS")
	envGhivediffReset     = os.Getenv("GHIVEDIFF_RESET")
	// GHIVEDIFF_CALC_FROM/TO: emit CASHOUT-CALC for all cashouts in the
	// [from,to] block range (drift localization without recompiling).
	envCalcFrom = parseUintEnv("GHIVEDIFF_CALC_FROM")
	envCalcTo   = parseUintEnv("GHIVEDIFF_CALC_TO")
	// GHIVEDIFF_CURATORS_FROM: emit CASHOUT-CURATOR for EVERY curator
	// claim from this block on — wholesale diff against the instrumented
	// hived's identical dump to localize curation-weight drift.
	envCuratorsFrom = parseUintEnv("GHIVEDIFF_CURATORS_FROM")
)

func parseUintEnv(k string) uint32 {
	v := os.Getenv(k)
	if v == "" {
		return 0
	}
	var n uint64
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + uint64(c-'0')
	}
	return uint32(n)
}

func calcTraceWindow(blockNum uint32) bool {
	return envCalcTo != 0 && blockNum >= envCalcFrom && blockNum <= envCalcTo
}

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
func processCommentCashouts(s *state.Session, blockTime, blockNum uint32, hooks *blockHookScratch, d *Database) {
	if !hardfork.SetForBlock(blockNum).Has(hardfork.HF7) {
		return
	}
	store := objects.EnsureCommentCashoutScheduleStore(s)
	// hived decays every reward fund's recent_claims and stamps
	// last_update = head_block_time on EVERY block at the top of
	// process_comment_cashout (database_comment.cpp:395-413), regardless
	// of whether any comment is due. This MUST run before the EarliestDue
	// early-return below: skipping it on no-cashout blocks decays
	// recent_claims in fewer/larger steps than hived (the decay is
	// elapsed-linear, so step granularity changes the integer result),
	// drifting the reward denominator and every downstream payout.
	if hardfork.SetForBlock(blockNum).Has(hardfork.HF17) {
		decayRecentClaims(s, blockTime, hardfork.SetForBlock(blockNum))
	}
	// Skip the rest of the hook when the earliest queued cashout is still
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
		// HF17_771+: decay fund.recent_claims linearly, then ADD every
		// due comment's curve to recent_claims BEFORE the cashout loop.
		// The augmented value is what each cashout reads — and we no
		// longer subtract per-cashout. Mirrors C++
		// database_comment.cpp:395-446 (decay + pre-add) +
		// database_comment.cpp:471 (cashout uses funds[id].recent_claims).
		// The per-block decay already ran above (decayRecentClaims); here
		// we only pre-add the due comments' curves (database_comment.cpp:437).
		preAddRecentClaims(s, *ready)
		// Snapshot fund.reward_balance ONCE here. Each cashout in the
		// block uses this same value as its denominator-side input —
		// matching C++'s `ctx.total_reward_fund_hive = funds[id].reward_balance`
		// where funds[id] is read once per cashout but the underlying
		// reward_balance isn't decremented until ALL block cashouts run
		// (database_comment.cpp:471 + :527 batch-subtract). Without this,
		// later same-block cashouts see a drained pool and under-pay.
		var snapshotBalance int64
		if fund, err := objects.GetRewardFund(s, objects.PostRewardFundName); err == nil {
			snapshotBalance = fund.RewardBalance
		}
		// HF17+: pay each due comment individually (matches
		// database_comment.cpp:466-481).
		for _, p := range *ready {
			processOneCashout(s, blockTime, blockNum, p.cashoutTime, p.commentID, p.author, p.permlink, curationPct, hooks, snapshotBalance, d)
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
			processOneCashout(s, blockTime, blockNum, sib.cashoutTime, sib.id, sib.author, sib.permlink, curationPct, hooks, 0, d)
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
// snapshotBalance > 0 selects the C++ HF17_771+ "snapshot fund balance
// at block start" path: each cashout in the block uses this value as
// its reward-fund denominator instead of fund.RewardBalance (which is
// being decremented across the loop). When 0 (or pre-HF17), the
// per-cashout fund.RewardBalance read is the legacy behavior.
func processOneCashout(s *state.Session, blockTime, blockNum, scheduledTime uint32, commentID uint64, author, permlink string, curationPct int64, hooks *blockHookScratch, snapshotBalance int64, d *Database) {
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

	if c.NetRshares <= 0 {
		// Frozen / no-payout comment: zero the on_payout fields AND
		// delete cv rows. C++ database_comment.cpp:417-424 deletes ALL
		// comment_vote rows at the end of cashout_helper, OUTSIDE the
		// `if (net_rshares > 0)` reward gate. Without this, NetRshares<=0
		// cashouts leave stale cv rows behind, so subsequent votes look
		// like recasts in our state but NEW votes in C++.
		//
		// NOTE: !AllowCurationRewards is NOT a skip condition. hived only
		// zeroes the CURATION pool for such comments (pay_curators,
		// database_comment.cpp:146-150); the AUTHOR is still paid
		// reward×(1-curation_pct) and the fund drains by that. Skipping the
		// whole payout left the fund over-funded — e.g. webdev/testing-more-
		// things at block 13,059,401 (author owed ~4697, paid 0), drifting
		// reward_balance and cascading into the post-HF19 sat-level reward
		// cluster + markrmorrisjr's claim failure at 13,060,794.
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
		// HF17_771+: prefer the block-start snapshot so same-block
		// cashouts see the same reward-pool value (matches C++).
		if snapshotBalance > 0 {
			fundBalance = snapshotBalance
		} else {
			fundBalance = fund.RewardBalance
		}
		shares2 = reward.Uint128FromBytes(fund.RecentClaims)
	} else {
		fundBalance = dgpo.TotalRewardFundHive
	}
	// C++ database_comment.cpp:417-424 always runs c.on_payout() (zero
	// accumulators) and deletes ALL comment_vote rows at end of cashout,
	// OUTSIDE the reward gate. Early-returning here without zero+delete
	// leaves stale rshares + cv rows that turn the next vote into a
	// recast-shaped path in Go but a fresh-vote-shaped path in C++.
	if fundBalance <= 0 {
		zero()
		deleteCommentVotes(s, author, permlink)
		return
	}
	if shares2.IsZero() {
		zero()
		deleteCommentVotes(s, author, permlink)
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
		zero()
		deleteCommentVotes(s, author, permlink)
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
	if traceAccount(author) || (envGhivediffAll != "" && ((blockNum >= 2888500 && blockNum <= 2910000) || (blockNum >= 9640246 && blockNum <= 9640246))) || calcTraceWindow(blockNum) {
		ghiveDiff("GHIVEDIFF CASHOUT-CALC block=%d author=%s permlink=%s id=%d cashoutTime=%d rshares=%d fundBalance=%d shares2_hi=%d shares2_lo=%d claimed_pre_clip=%d\n",
			blockNum, author, permlink, c.ID, scheduledTime, c.NetRshares, fundBalance, shares2.Hi, shares2.Lo, claimed)
	}
	// Mirror C++ get_rshare_reward (libraries/chain/util/reward.cpp:24-29)
	// EXACTLY: (1) dust-check on RAW claimed first, (2) min(claimed,
	// max_hive). No clip against fund balance — C++ never does this in
	// get_rshare_reward; the fund decrement happens later via the caller.
	// Prior order (min → fund-clip → dust-check) could zero payouts that
	// C++ paid and vice-versa near the dust threshold, and the fund clip
	// silently under-paid the last cashouts of a heavily-loaded block.
	feedHistoryFetched, feedHistoryOK := objects.GetFeedHistory(s)
	feedReady := feedHistoryOK == nil && !priceIsZero(feedHistoryFetched.CurrentMedianHistory)
	// HIVE_MIN_PAYOUT_HBD = 20 satoshi (= 0.020 HBD). Dust check on RAW
	// payout, BEFORE the max-payout min-clip.
	if claimed > 0 && feedReady {
		if convertHiveToHBD(claimed, feedHistoryFetched.CurrentMedianHistory) < 20 {
			claimed = 0
		}
	}
	// MaxAcceptedPayout is denominated in HBD; convert via the median
	// feed for the HIVE-sat min-clip. Mirrors libraries/chain/util/reward.cpp:27-29
	// + database_comment.cpp:219: `max_hive = to_hive(current_hive_price,
	// max_hbd); payout = min(payout, max_hive)`.
	maxClaim := c.MaxAcceptedPayout
	if feedReady {
		// C++ has NO fallback when the conversion floors to zero
		// (util/reward.cpp:27-29: payout = min(payout, to_hive(price,
		// max_hbd))) — a sub-sat max_accepted_payout (e.g. 0.001 HBD)
		// legitimately zeroes the whole payout. A previous defensive
		// `maxClaim <= 0 → raw HBD amount` fallback paid 1 HIVE-sat on
		// the.queen/w (max=0.001 HBD) at block 24,211,835 where hived
		// pays 0, draining the reward fund 1 sat low and flipping
		// knife-edge claims downstream (joeparys at 24,222,484).
		maxClaim = convertHBDToHive(c.MaxAcceptedPayout, feedHistoryFetched.CurrentMedianHistory)
	}
	if claimed > maxClaim {
		claimed = maxClaim
	}
	// POST-dust/post-clip claimed — directly comparable to the
	// instrumented hived's CASHOUT-CALC `claimed` (which is post-dust).
	// Diff this against hived to localize per-comment over/under-drain
	// under the HF21 convergent curves (the pre-clip field above logs
	// the raw curve value, which differs from hived's post-dust value
	// for dust comments and masks the real divergence).
	if traceAccount(author) || (envGhivediffAll != "" && ((blockNum >= 2888500 && blockNum <= 2910000) || (blockNum >= 9640246 && blockNum <= 9640246))) || calcTraceWindow(blockNum) {
		ghiveDiff("GHIVEDIFF CASHOUT-CLAIMED-FINAL block=%d author=%s permlink=%s claimed_final=%d\n",
			blockNum, author, permlink, claimed)
	}

	// Pre-HF17 only: subtract this comment's curve from
	// dgpo.total_reward_shares2 (mirrors
	// database_comment.cpp:328-329 adjust_rshares2(curve, 0)). HF17_771+
	// switches to the per-block pre-add model handled by
	// decayPreAddRecentClaims; the per-cashout subtract is removed there
	// so recent_claims stays consistent with C++.
	if !usingFund {
		shares2 = shares2.Sub(commentCurve)
		dgpo.TotalRewardShares2 = reward.Uint128ToBytes(shares2)
	}

	if claimed <= 0 {
		if !usingFund {
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

	// HF17+ honors the reward_fund's per-curation share; pre-HF17 we
	// fall back to the legacy curationPct gate (50/50 pre-HF8, 25/75
	// post-HF8). Compute the split BEFORE draining the pool: when curation
	// is disallowed the curation share is forfeited (never paid, never
	// drained), so only authorPool comes out of the fund.
	if usingFund && fund.PercentCurationRewards > 0 {
		curationPct = int64(fund.PercentCurationRewards)
	}
	curatorPool := claimed * curationPct / 10000
	authorPool := claimed - curatorPool

	// Decrement the source pool. hived drains hive_awarded = claimed_reward,
	// which is the full reward normally but only author_tokens (= claimed -
	// curation_tokens) when allows_curation_rewards is false — the forfeited
	// curation stays in the fund.
	drain := claimed
	if !c.AllowCurationRewards {
		drain = authorPool
	}
	if usingFund {
		fund.RewardBalance -= drain
		objects.PutRewardFund(s, &fund)
	} else {
		dgpo.TotalRewardFundHive -= drain
		objects.PutDGPO(s, &dgpo)
	}

	// Mirror C++ cashout_comment_helper order (database_comment.cpp:259-310):
	// CURATORS FIRST (so authors / beneficiaries see the post-curator
	// vesting share price), then beneficiaries, then author. Each
	// create_vesting2 call mutates total_vesting_fund_hive and
	// total_vesting_shares, so the per-recipient payout depends on the
	// order. Earlier Go paid beneficiaries before curators, which drifted
	// share prices for any post with both — dormant pre-HF17 (no
	// beneficiaries) but visible later.
	curatorRemainder := int64(0)
	curatorsVestingPayout := int64(0)
	if c.AllowCurationRewards && curatorPool > 0 {
		curatorRemainder, curatorsVestingPayout = distributeCuratorPool(s, c.Author, c.Permlink, curatorPool, hooks, d)
	}
	// When curation is disallowed, curatorRemainder stays 0 — the curation
	// share is forfeited (already excluded from `drain` above), NOT forwarded
	// to the author. Mirrors hived pay_curators zeroing max_rewards so
	// curation_remainder = 0 and author_tokens = reward - curation_tokens.
	hf := hardfork.SetForBlock(blockNum)
	if !hf.Has(hardfork.HF20) && curatorRemainder > 0 {
		authorPool += curatorRemainder
	} else if curatorRemainder > 0 {
		// HF20__1877: the unclaimed curation remainder is no longer
		// forwarded to the author — and it never leaves the fund. C++
		// pays out of a temp reward_balance and writes the residue back
		// (database_comment.cpp:504-509); we drained `claimed` up front,
		// so restitute the remainder. Intra-block claim math uses the
		// block-start snapshot, so the timing is equivalent.
		if usingFund {
			fund.RewardBalance += curatorRemainder
			objects.PutRewardFund(s, &fund)
		} else {
			dgpo.TotalRewardFundHive += curatorRemainder
			objects.PutDGPO(s, &dgpo)
		}
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
		// hived database::is_treasury (database.cpp:374) matches BOTH the
		// current AND obsolete treasury names: NEW_HIVE_TREASURY_ACCOUNT
		// ("hive.fund") || OBSOLETE_TREASURY_ACCOUNT ("steem.dao"). A comment
		// may name the obsolete "steem.dao" as a DHF beneficiary even at HF24+;
		// hived still routes the WHOLE share to the CURRENT treasury as HBD.
		// We previously only matched the *current* name (treasuryName), so such
		// comments fell through to the 50/50 split below — under-burning
		// current_supply by share/2 and over-vesting. ROOT of the khaiyoui
		// 48,032,488 cs+11/hbd-2 divergence (cascades via virtual_supply →
		// new_hive floor → vesting_reward → F/S short).
		if hfBene.Has(hardfork.HF21) && (b.Account == "hive.fund" || b.Account == "steem.dao") {
			// Treasury branch: convert HIVE→HBD at current median.
			fh, ferr := objects.GetFeedHistory(s)
			if ferr != nil || priceIsZero(fh.CurrentMedianHistory) {
				totalBeneficiary += share
				continue
			}
			hbdPayout := convertHiveToHBD(share, fh.CurrentMedianHistory)
			// Credit the CURRENT treasury (mirrors hived adjust_balance(
			// get_treasury())), NOT b.Account — the beneficiary may name the
			// obsolete "steem.dao" but the HBD goes to the live treasury.
			tAcct, terr := objects.GetAccount(s, auth.AccountName(treasuryName))
			if terr == nil {
				if envGhivediffAll != "" && calcTraceWindow(blockNum) {
					fmt.Fprintf(os.Stderr, "GHIVEDIFF BENE-TREASURY block=%d author=%s benefactor=%s treasury=%s hbd_before=%d payout=%d share=%d\n",
						blockNum, c.Author, b.Account, treasuryName, tAcct.HBDBalance, hbdPayout, share)
				}
				objects.AdjustHBDBalance(s, &tAcct, blockTime, hbdPayout)
				objects.PutAccount(s, &tAcct)
			}
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
		// Hived (database_comment.cpp:244-266): pre-HF20, beneficiaries
		// receive 100 % of their `benefactor_tokens` slice as VESTS via
		// create_vesting2 — no HBD/HIVE leg at all. The HBD split
		// (`percent_hbd / (2 × 100 %)`) only kicks in at HF20. We were
		// splitting unconditionally, which over the HF17→HF20 window
		// (block 10,629,455 → 26,256,743) under-credited every
		// beneficiary's reward_vests by exactly one HIVE-of-VESTS per
		// cashout slice (≈2,073,317 VESTS at HF17-era share price) and
		// over-credited reward_hbd by the matching amount. This is the
		// dominant systemic drift the auto-correction loop has been
		// papering over.
		hbdSlice := int64(0)
		vestingSlice := share
		if hfBene.Has(hardfork.HF20) {
			hbdSlice = share * int64(c.PercentHbd) / 20000
			vestingSlice = share - hbdSlice
		}
		// HF17+ routes author/beneficiary payouts through reward_* fields
		// that the user later moves to liquid via claim_reward_balance.
		toReward := hfBene.Has(hardfork.HF17)
		if hbdSlice > 0 {
			payCreateHbd(s, &bAcct, blockTime, hbdSlice, toReward)
			objects.PutAccount(s, &bAcct)
		}
		if vestingSlice > 0 {
			benevests, err := CreateVesting_via_chain(s, b.Account, vestingSlice, toReward)
			if err != nil {
				ghiveDiff("GHIVEDIFF CREATE-VESTING-FAIL site=comment_cashout.beneficiary block=%d author=%s permlink=%s beneficiary=%s amount=%d err=%v\n",
					blockNum, c.Author, c.Permlink, b.Account, vestingSlice, err)
			} else if envGhivediffAll != "" && calcTraceWindow(blockNum) {
				ghiveDiff("GHIVEDIFF BENE-VEST block=%d author=%s permlink=%s benefactor=%s vesting_slice=%d vests=%d\n",
					blockNum, c.Author, c.Permlink, b.Account, vestingSlice, benevests)
			}
		}
		if traceAccount(string(b.Account)) {
			ghiveDiff("GHIVEDIFF CASHOUT-BENEFICIARY block=%d author=%s permlink=%s beneficiary=%s weight=%d author_pool=%d share=%d hbd_slice=%d vesting_slice=%d percent_hbd=%d\n",
				blockNum, c.Author, c.Permlink, b.Account, b.Weight, authorPool, share, hbdSlice, vestingSlice, c.PercentHbd)
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
	if traceAccount(author) || (envGhivediffAll != "" && ((blockNum >= 2888500 && blockNum <= 2910000) || (blockNum >= 9640246 && blockNum <= 9640246))) || calcTraceWindow(blockNum) {
		ghiveDiff("GHIVEDIFF CASHOUT-AUTHOR block=%d author=%s permlink=%s author_tokens=%d hbd_hive=%d vesting_hive=%d curator_remainder=%d\n",
			blockNum, author, permlink, authorPool, hbdHive, vestingHive, curatorRemainder)
	}
	hfHere := hardfork.SetForBlock(blockNum)
	authorToReward := hfHere.Has(hardfork.HF17)
	authorHbdPaid, authorHivePaid := int64(0), int64(0)
	if hbdHive > 0 {
		if a, gerr := objects.GetAccount(s, author); gerr == nil {
			authorHbdPaid, authorHivePaid = payCreateHbd(s, &a, blockTime, hbdHive, authorToReward)
			objects.PutAccount(s, &a)
		}
	}
	authorVests := int64(0)
	if vestingHive > 0 {
		avests, err := CreateVesting_via_chain(s, author, vestingHive, authorToReward)
		authorVests = avests
		if err != nil {
			ghiveDiff("GHIVEDIFF CREATE-VESTING-FAIL site=comment_cashout.author block=%d author=%s permlink=%s amount=%d err=%v\n",
				blockNum, author, permlink, vestingHive, err)
		} else if envGhivediffAll != "" && calcTraceWindow(blockNum) {
			ghiveDiff("GHIVEDIFF AUTHOR-VEST block=%d author=%s permlink=%s vesting_hive=%d vests=%d\n",
				blockNum, author, permlink, vestingHive, avests)
		}
	}
	// hived pushes author_reward unconditionally once a comment reaches the
	// paying branch (database_comment.cpp:291-299) — even when a leg is zero,
	// because the op is how a consumer learns the payout HAPPENED. Gating it
	// on a non-zero leg would drop the op for posts paid entirely in one
	// currency. curators_vesting_payout is carried here so a consumer can
	// account the whole claim from this one op without summing every
	// curation_reward (hived's own stated reason, database_comment.cpp:228-230).
	d.pushVOp(op.AuthorReward{
		Author:                auth.AccountName(author),
		Permlink:              permlink,
		HbdPayout:             vopHbd(authorHbdPaid),
		HivePayout:            vopHive(authorHivePaid),
		VestingPayout:         vopVests(authorVests),
		CuratorsVestingPayout: vopVests(curatorsVestingPayout),
		PayoutMustBeClaimed:   authorToReward,
	})

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

	// HF19+ removes only the comment_cashout_object after payout
	// (database_comment.cpp:478-481 `remove(*_current)` — _current is the
	// comment_cashout_object). The comment_object IDENTITY PERSISTS, so
	// post-cashout replies still resolve their parent and the comment can
	// still be referenced (vote/edit, gated by WasPaid+CashoutTime).
	//
	// We must NOT delete our unified Comment row here: it carries the
	// identity (author/permlink/parent/root) too. Deleting it broke every
	// reply/vote/edit referencing a paid comment from HF19 (block
	// 12,988,978) onward — first failure at 12,988,981, parent
	// "aatablantel/buenos-dias" not found. The heavy cashout fields are
	// already cleared above (zero() + deleteCommentVotes), so the retained
	// row mirrors hived's lightweight comment_object. WasPaid=true +
	// CashoutTime=0 (set during this cashout) make the vote/comment/delete
	// evaluators treat it as fully-paid, matching hived's
	// removed-comment_cashout_object semantics.
	_ = rescheduleTime
}

// decayPreAddRecentClaims mirrors hived's per-block recent_claims
// maintenance for HF17_771+ (database_comment.cpp:395-446):
//
//  1. Decay each fund's recent_claims by elapsed / decay_time, where
//     decay_time is 30d pre-HF19 and 15d post-HF19.
//  2. For each comment cashing out this block with NetRshares > 0,
//     ADD its curve value to recent_claims BEFORE any cashout runs.
//
// Each cashout in the loop then reads the augmented recent_claims as a
// constant denominator. The per-cashout subtract that worked pre-HF17
// is removed in the HF17_771+ branch above.
//
// Without this hook, votes wrongly fed recent_claims via the legacy
// adjust_rshares2 path and per-cashout subtractions drained it,
// drifting our recent_claims ~1%+ relative to hived's and shrinking
// every post-HF17 author/curator payout — surface symptom is the
// `claim_reward_balance: insufficient balance` mismatch first hit at
// block 10,635,691 for account "ocrdu".
func decayRecentClaims(s *state.Session, blockTime uint32, hf hardfork.Set) {
	fund, err := objects.GetRewardFund(s, objects.PostRewardFundName)
	if err != nil {
		return
	}
	const (
		decayHF17Sec uint32 = 30 * 24 * 60 * 60
		decayHF19Sec uint32 = 15 * 24 * 60 * 60
	)
	decaySec := decayHF17Sec
	if hf.Has(hardfork.HF19) {
		decaySec = decayHF19Sec
	}
	if fund.LastUpdate != 0 && blockTime > fund.LastUpdate {
		elapsed := blockTime - fund.LastUpdate
		if elapsed >= decaySec {
			// More than a full window — clear. C++ comments this as
			// "should never happen unless chain inactive for decay_time".
			fund.RecentClaims = reward.Uint128ToBytes(math128.U128{})
		} else {
			cur := reward.Uint128FromBytes(fund.RecentClaims)
			// recent_claims -= recent_claims * elapsed / decay_time.
			// 128 × 64 → up to 192 bits, divide by 64 → 128. Use the
			// existing U128 × u64 / u64 helpers.
			shed := mulDivU128(cur, uint64(elapsed), uint64(decaySec))
			cur = cur.Sub(shed)
			fund.RecentClaims = reward.Uint128ToBytes(cur)
		}
	}
	fund.LastUpdate = blockTime
	objects.PutRewardFund(s, &fund)
}

// preAddRecentClaims augments fund.recent_claims with the curve value of
// every due comment with NetRshares > 0 BEFORE the cashout loop reads it
// (database_comment.cpp:437). The per-block decay+stamp already ran in
// decayRecentClaims, so this only adds; the augmented value is the
// constant denominator each same-block cashout divides by.
func preAddRecentClaims(s *state.Session, ready []blockHookCashoutPending) {
	fund, err := objects.GetRewardFund(s, objects.PostRewardFundName)
	if err != nil {
		return
	}
	cur := reward.Uint128FromBytes(fund.RecentClaims)
	for _, p := range ready {
		c, ok := objects.GetCommentRef(s, p.author, p.permlink)
		if !ok || c.NetRshares <= 0 {
			continue
		}
		cur = cur.Add(evaluateCurveForRewardFund(c.NetRshares, &fund))
	}
	fund.RecentClaims = reward.Uint128ToBytes(cur)
	objects.PutRewardFund(s, &fund)
}

// mulDivU128 returns floor(a * b / d) for U128 a and uint64 b, d.
// Used by the recent_claims decay step where elapsed/decay_time scales
// a U128 accumulator.
func mulDivU128(a math128.U128, b, d uint64) math128.U128 {
	if d == 0 || b == 0 || a.IsZero() {
		return math128.U128{}
	}
	// (hi, lo) = a × b — produces up to 192 bits. Reduce by dividing
	// the top half first, propagating remainder into the bottom.
	hiHi, hiLo := bits.Mul64(a.Hi, b)
	loHi, loLo := bits.Mul64(a.Lo, b)
	midHi, carry := bits.Add64(hiLo, loHi, 0)
	hiHi += carry
	// Now full product: (hiHi << 128) | (midHi << 64) | loLo, as 192-bit
	// little-endian. Divide by d:
	q2, r2 := bits.Div64(0, hiHi, d)
	q1, r1 := bits.Div64(r2, midHi, d)
	q0, _ := bits.Div64(r1, loLo, d)
	if q2 != 0 {
		// Overflow: result doesn't fit in 128 bits. Cap.
		return math128.U128{Hi: ^uint64(0), Lo: ^uint64(0)}
	}
	return math128.U128{Hi: q1, Lo: q0}
}

// evaluateCurveForRewardFund applies the fund's selected reward curve
// to the comment's net_rshares. Wrapper around evaluateRewardCurveU128
// so decayPreAddRecentClaims doesn't reach into the cashout helper.
func evaluateCurveForRewardFund(netRshares int64, fund *objects.RewardFund) math128.U128 {
	if netRshares <= 0 {
		return math128.U128{}
	}
	return evaluateRewardCurveU128(netRshares, fund.AuthorRewardCurve, fund.ContentConstant)
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
		// Mirror C++ util/reward.cpp:58 — sqrt uses fc::uint128_approx_sqrt
		// (mantissa-MSB approximation that biases UPWARD between perfect
		// squares), NOT exact Newton's-method sqrt. The approximation is
		// implemented at internal/math128/math128.go:154-195 (ApproxSqrt).
		// Previously called exact chainISqrt64 → results diverged from
		// hived by ~1-2 units for small inputs, up to hundreds at
		// rshares ~ 2^40, biasing every author/curator payout on
		// sqrt-curve posts (HF17→HF19 default + post-HF19_1052 with
		// reward_fund.CurationRewardCurve == square_root).
		return math128.U128{Lo: math128.U128{Lo: r}.ApproxSqrt()}
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
		// Mirror C++ util/reward.cpp:69 — denominator sqrt uses
		// approx_sqrt; result = rshares / approx_sqrt(rshares + 2s).
		den := math128.U128{Lo: r + 2*s}.ApproxSqrt()
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
// hbd_print_rate IS tracked: update_virtual_supply recomputes it each block
// (virtual_supply.go) and the body below reads the live DGPO value. 0 is a
// valid rate (0% to HBD) — see the guard-bug note at the read site.
func payCreateHbd(s *state.Session, a *objects.Account, blockTime uint32, hiveAmount int64, toReward bool) (hbdPaid, hivePaid int64) {
	if hiveAmount <= 0 {
		return 0, 0
	}
	fh, err := objects.GetFeedHistory(s)
	if err != nil || priceIsZero(fh.CurrentMedianHistory) {
		// No feed → everything pays as liquid HIVE (or reward HIVE).
		if toReward {
			a.RewardHive += hiveAmount
		} else {
			a.Balance += hiveAmount
		}
		return 0, hiveAmount
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
	if toReward {
		// HF17+ reward-balance path: credit reward_hbd_balance and
		// reward_hive_balance directly (no hbd_seconds accrual since
		// reward balances don't earn interest). User claims later via
		// claim_reward_balance.
		a.RewardHBD += hbdAmount
		a.RewardHive += toHive
	} else {
		objects.AdjustHBDBalance(s, a, blockTime, hbdAmount)
		a.Balance += toHive
	}

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
	return hbdAmount, toHive
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
func distributeCuratorPool(s *state.Session, author, permlink string, pool int64, hooks *blockHookScratch, d *Database) (int64, int64) {
	// Read-only access: only inspecting c.TotalVoteWeight to gate +
	// derive the denominator for the curator-share math.
	c, ok := objects.GetCommentRef(s, author, permlink)
	if !ok {
		return pool, 0
	}
	if c.TotalVoteWeight == 0 || pool <= 0 {
		return pool, 0
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
			// Capture the voter's account creation time for the equal-weight
			// tie-break below (hived orders equal-weight curators by voter
			// account_id, i.e. creation order — not by voter name).
			var created uint32
			if av, ok := objects.GetAccountRef(s, cv.Voter); ok {
				created = av.Created
			}
			*pays = append(*pays, CuratorPay{Voter: string(cv.Voter), Weight: cv.CurationWeight, Share: s64, Created: created})
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
		// Equal weight: hived's pay_curators cmp tie-breaks by
		// obj->get_voter() — the voter's account_id, i.e. account CREATION
		// order — NOT the voter name. Ordering by name instead pays
		// equal-weight curators in a different order; since each CreateVesting
		// mutates the reward-vesting pool price, the per-curator vests drift by
		// a satoshi, surfacing as e.g. leo.voter's reward_vests coming up 1 sat
		// short at claim_reward_balance block 52,378,769. Approximate account_id
		// order by (Created ASC, Name ASC), same tie-break as pay_liquidity_reward
		// (7838fd8).
		if a.Created != b.Created {
			if a.Created < b.Created {
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
	// Total VESTS handed to curators. author_reward carries this as
	// curators_vesting_payout, so the author's own vop reports what the
	// curators took off the same claim — hived threads the identical value
	// out of pay_curators (database_comment.cpp:174, total_paid_reward).
	paidVests := int64(0)
	// blockNum at the start of distributeCuratorPool is the dgpo's
	// HeadBlockNumber (mutated by ApplyBlock pre-hooks). GetDGPORef
	// skips the ~1 KB struct value-copy that GetDGPO would do per
	// cashout (~500K cashouts in the 8M window).
	var headBlockNum uint32
	if dgpoRef, ok := objects.GetDGPORef(s); ok {
		headBlockNum = dgpoRef.HeadBlockNumber
	}
	// HF17_659 (block 10,629,455 mainnet) flips curator vests to the
	// reward_balance — claimable later via claim_reward_balance —
	// instead of crediting vesting_shares directly.
	hf := hardfork.SetForBlock(headBlockNum)
	toReward := hf.Has(hardfork.HF17)
	for _, p := range *pays {
		curVests, err := CreateVesting_via_chain(s, p.Voter, p.Share, toReward)
		if err != nil {
			// Skip `paid += p.Share` on failure — the curator pool stays
			// accounted for the actual paid amount so the residual flows
			// back correctly to the author (matches C++ which would have
			// asserted the voter exists).
			ghiveDiff("GHIVEDIFF CREATE-VESTING-FAIL site=comment_cashout.curator block=%d author=%s permlink=%s voter=%s amount=%d err=%v\n",
				headBlockNum, author, permlink, p.Voter, p.Share, err)
			continue
		}
		if traceAccount(p.Voter) || traceAccount(author) ||
			(envCuratorsFrom > 0 && headBlockNum >= envCuratorsFrom) {
			ghiveDiff("GHIVEDIFF CASHOUT-CURATOR block=%d author=%s permlink=%s voter=%s weight=%d total_weight=%d pool=%d claim_hive=%d vests=%d\n",
				headBlockNum, author, permlink, p.Voter, p.Weight, c.TotalVoteWeight, pool, p.Share, curVests)
		}
		paid += p.Share
		paidVests += curVests
		// hived builds the vop BEFORE create_vesting2 and fills in the
		// reward inside its callback, so the VESTS reported are the ones
		// that call minted at the then-current share price — which moves
		// with every curator paid. Pushing after the call with the value it
		// returned is the same number.
		d.pushVOp(op.CurationReward{
			Curator:             auth.AccountName(p.Voter),
			Reward:              vopVests(curVests),
			Author:              auth.AccountName(author),
			Permlink:            permlink,
			PayoutMustBeClaimed: toReward,
		})
	}
	if paid > pool {
		return 0, paidVests
	}
	return pool - paid, paidVests
}
