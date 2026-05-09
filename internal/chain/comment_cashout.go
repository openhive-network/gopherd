package chain

import (
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"

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
func processCommentCashouts(s *state.Session, blockTime, blockNum uint32) {
	if !hardfork.SetForBlock(blockNum).Has(hardfork.HF7) {
		return
	}
	type pending struct {
		cashoutTime uint32
		commentID   uint64
		author      string
		permlink    string
	}
	var ready []pending

	// Schedule keys are (cashout_time_BE, comment_id_BE). Iteration order
	// matches C++'s composite_key<cashout_time, comment_id>: smallest time
	// first, then smallest id (insertion order) within the same time. The
	// (author, permlink) pair sits in the entry's value.
	_ = s.Range(objects.CommentCashoutScheduleTable, state.PrimaryIndex, nil,
		func(key, val []byte) (bool, error) {
			if len(key) < 4+8 {
				return true, nil
			}
			t := uint32(key[0])<<24 | uint32(key[1])<<16 | uint32(key[2])<<8 | uint32(key[3])
			if t > blockTime {
				return false, nil
			}
			id := uint64(0)
			for i := 0; i < 8; i++ {
				id = (id << 8) | uint64(key[4+i])
			}
			author, permlink := objects.DecodeCommentCashoutScheduleValue(val)
			ready = append(ready, pending{
				cashoutTime: t,
				commentID:   id,
				author:      string(author),
				permlink:    permlink,
			})
			return true, nil
		})

	curationPct := curationPercentForBlock(blockNum)
	hf := hardfork.SetForBlock(blockNum)
	if hf.Has(hardfork.HF17) {
		// HF17+: pay each due comment individually (matches
		// database_comment.cpp:466-481).
		for _, p := range ready {
			processOneCashout(s, blockTime, blockNum, p.cashoutTime, p.commentID, p.author, p.permlink, curationPct)
		}
		return
	}

	// Pre-HF17 thread-cashout grouping (database_comment.cpp:485-507):
	// for each ready entry, find the discussion ROOT and pay every
	// comment under that root_id together. Already-paid comments are
	// skipped via a per-call dedup set, so we never double-pay even
	// when multiple replies in the same thread land in `ready`.
	paid := make(map[string]struct{}, len(ready))
	for _, p := range ready {
		c, cerr := objects.GetComment(s, p.author, p.permlink)
		if cerr != nil {
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
		type sibling struct {
			author      string
			permlink    string
			cashoutTime uint32
			id          uint64
		}
		var siblings []sibling
		_ = objects.RangeCommentsByRoot(s, auth.AccountName(rootAuthor), rootPermlink,
			func(sib *objects.Comment) (bool, error) {
				siblings = append(siblings, sibling{
					author:      string(sib.Author),
					permlink:    sib.Permlink,
					cashoutTime: sib.CashoutTime,
					id:          sib.ID,
				})
				return true, nil
			})
		sort.Slice(siblings, func(i, j int) bool { return siblings[i].id < siblings[j].id })
		for _, sib := range siblings {
			key := sib.author + "/" + sib.permlink
			if _, dup := paid[key]; dup {
				continue
			}
			paid[key] = struct{}{}
			processOneCashout(s, blockTime, blockNum, sib.cashoutTime, sib.id, sib.author, sib.permlink, curationPct)
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
func processOneCashout(s *state.Session, blockTime, blockNum, scheduledTime uint32, commentID uint64, author, permlink string, curationPct int64) {
	objects.DeleteCommentCashoutSchedule(s, scheduledTime, commentID)

	c, err := objects.GetComment(s, author, permlink)
	if err != nil {
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
	if dgpo, derr := objects.GetDGPO(s); derr == nil {
		hf := hardfork.SetForBlock(dgpo.HeadBlockNumber)
		if !hf.Has(hardfork.HF17) && isRoot && hf.Has(hardfork.HF12) && !wasPaidPre {
			rescheduleTime = blockTime + hiveSecondCashoutWindow
		}
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
		objects.PutComment(s, &c)
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
	if shares2.Sign() <= 0 {
		return
	}

	// curve(net_rshares) — uses fund's content_constant + curve when the
	// reward_fund is in play; otherwise the legacy quadratic with
	// HIVE_CONTENT_CONSTANT_HF0.
	var commentCurve *big.Int
	if usingFund {
		commentCurve = evaluateRewardCurve(c.NetRshares, fund.AuthorRewardCurve, fund.ContentConstant)
	} else {
		commentCurve = reward.EvaluateRewardCurveQuadratic(c.NetRshares)
	}
	if commentCurve.Sign() <= 0 {
		return
	}

	// reward_weight haircut (HF12-HF17 ROOT posts only). Mirrors
	// libraries/chain/util/reward.cpp:18: `claim = (claim × reward_weight)
	// / HIVE_100_PERCENT`. The weighted claim drives `payout`, but the
	// unweighted curve still drives the `shares2` subtraction below
	// (C++ database_comment.cpp:380 calls adjust_rshares2 with the raw
	// curve). RewardWeight==0 means the field hasn't been set
	// (legacy/replies/post-HF17) — treat as HIVE_100_PERCENT (no haircut).
	weightedCurve := commentCurve
	if c.RewardWeight != 0 && c.RewardWeight < 10000 {
		weightedCurve = new(big.Int).Mul(commentCurve, big.NewInt(int64(c.RewardWeight)))
		weightedCurve.Quo(weightedCurve, big.NewInt(10000))
	}

	// payout = fundBalance × weightedCurve / shares2.
	payoutBig := new(big.Int).Mul(big.NewInt(fundBalance), weightedCurve)
	payoutBig.Quo(payoutBig, shares2)
	if !payoutBig.IsInt64() {
		payoutBig.SetInt64(int64(^uint64(0) >> 1))
	}
	claimed := payoutBig.Int64()
	if traceAccount(author) || (envGhivediffAll != "" && blockNum >= 2888500 && blockNum <= 2910000) {
		hi := uint64(0)
		lo := uint64(0)
		if shares2.IsUint64() {
			lo = shares2.Uint64()
		} else {
			b := shares2.Bytes()
			pad := make([]byte, 16)
			if len(b) > 16 {
				b = b[len(b)-16:]
			}
			copy(pad[16-len(b):], b)
			for i := 0; i < 8; i++ {
				hi = hi<<8 | uint64(pad[i])
				lo = lo<<8 | uint64(pad[i+8])
			}
		}
		ghiveDiff("GHIVEDIFF CASHOUT-CALC block=%d author=%s permlink=%s id=%d cashoutTime=%d rshares=%d fundBalance=%d shares2_hi=%d shares2_lo=%d claimed_pre_clip=%d\n",
			blockNum, author, permlink, c.ID, scheduledTime, c.NetRshares, fundBalance, hi, lo, claimed)
	}
	// MaxAcceptedPayout is denominated in HBD (HBD_asset in C++); to clip
	// against `claimed` (HIVE-sat), convert via the median feed.
	// Mirrors libraries/chain/util/reward.cpp:27-29 + database_comment.cpp:219:
	// `max_hive = to_hive(current_hive_price, max_hbd); payout = min(payout, max_hive)`.
	maxClaim := c.MaxAcceptedPayout
	if fh, ferr := objects.GetFeedHistory(s); ferr == nil && !priceIsZero(fh.CurrentMedianHistory) {
		maxClaim = convertHBDToHive(c.MaxAcceptedPayout, fh.CurrentMedianHistory)
		if maxClaim <= 0 {
			maxClaim = c.MaxAcceptedPayout // fallback if conversion yields 0
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
	// This prevents 1-satoshi rounding payouts from cluttering vops.
	if claimed > 0 {
		if fh, ferr := objects.GetFeedHistory(s); ferr == nil && !priceIsZero(fh.CurrentMedianHistory) {
			if convertHiveToHBD(claimed, fh.CurrentMedianHistory) < 20 {
				claimed = 0
			}
		}
	}

	// adjust_rshares2 / recent_claims: drop this comment's curve from the
	// running total. Pre-HF17 the cashout helper explicitly calls
	// `adjust_rshares2(curve(net_rshares), 0)` at
	// database_comment.cpp:350-351; HF17+ the per-fund recent_claims
	// snapshot is decremented analogously.
	// Mirror C++ database.cpp:1673-1674 — uint128 wraps mod 2^128 on
	// transient underflow. Uint128ToBytes performs the mod, no clamp.
	shares2.Sub(shares2, commentCurve)
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
		curatorRemainder = distributeCuratorPool(s, c.Author, c.Permlink, curatorPool)
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
			if dgpo, derr := objects.GetDGPO(s); derr == nil {
				adjustSupplyHive(s, &dgpo, -share)
				adjustSupplyHbd(s, &dgpo, hbdPayout)
				objects.PutDGPO(s, &dgpo)
			}
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

// evaluateRewardCurveBoundedCuration mirrors C++ reward.cpp:50-52
// `bounded_curation`: (low64(rshares) << 64) / (rshares + 2s).
// Used when a reward_fund.curve == bounded_curation. In Go we hold
// rshares in int64, so low64(rshares) == rshares for nonnegative
// values, and the left-shift simply scales numerator by 2^64.
func evaluateRewardCurveBoundedCuration(r, s *big.Int) *big.Int {
	twoS := new(big.Int).Lsh(s, 1)
	num := new(big.Int).Lsh(r, 64)
	den := new(big.Int).Add(twoS, r)
	return new(big.Int).Quo(num, den)
}

// evaluateRewardCurve dispatches between the curve types selectable
// for a reward_fund_object. Mirrors libraries/chain/util/reward.cpp:
//
//	quadratic:   (rshares + s)² - s²
//	bounded:     same as quadratic with content_constant baked in
//	linear:      rshares
//	square_root: floor(sqrt(rshares))
//	convergent_*: rshares² / (s + rshares); rshares / (s + rshares)
func evaluateRewardCurve(rshares int64, curve objects.CurveID, contentConstant int64) *big.Int {
	if rshares <= 0 {
		return new(big.Int)
	}
	r := big.NewInt(rshares)
	s := big.NewInt(contentConstant)
	if s.Sign() <= 0 {
		s = big.NewInt(2_000_000_000_000) // HIVE_CONTENT_CONSTANT_HF0
	}
	switch curve {
	case objects.CurveLinear:
		return new(big.Int).Set(r)
	case objects.CurveSquareRoot:
		out := new(big.Int).Sqrt(r)
		return out
	case objects.CurveBoundedCurve:
		return evaluateRewardCurveBoundedCuration(r, s)
	case objects.CurveConvergentLinear:
		// C++ reward.cpp:63 — ((r+s)² - s²) / (r + 4s).
		// Algebraically equivalent to r(r+2s)/(r+4s).
		rPlusS := new(big.Int).Add(r, s)
		num := new(big.Int).Mul(rPlusS, rPlusS)
		s2 := new(big.Int).Mul(s, s)
		num.Sub(num, s2)
		fourS := new(big.Int).Lsh(s, 2)
		den := new(big.Int).Add(r, fourS)
		return new(big.Int).Quo(num, den)
	case objects.CurveConvergentSquareRoot:
		// C++ reward.cpp:69 — rshares / sqrt(rshares + 2s).
		twoS := new(big.Int).Lsh(s, 1)
		den := new(big.Int).Sqrt(new(big.Int).Add(r, twoS))
		if den.Sign() == 0 {
			return new(big.Int)
		}
		return new(big.Int).Quo(r, den)
	default:
		// quadratic: (r+s)² - s²
		rPlusS := new(big.Int).Add(r, s)
		out := new(big.Int).Mul(rPlusS, rPlusS)
		s2 := new(big.Int).Mul(s, s)
		out.Sub(out, s2)
		return out
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
	// supply rises past hbd_start_percent. We re-read DGPO here to pick
	// up the latest value.
	hbdPrintRate := int64(10000)
	if dgpo, derr := objects.GetDGPO(s); derr == nil && dgpo.HBDPrintRate > 0 {
		hbdPrintRate = int64(dgpo.HBDPrintRate)
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
	if dgpo, derr := objects.GetDGPO(s); derr == nil {
		adjustSupplyHive(s, &dgpo, -toHbdHive)
		adjustSupplyHbd(s, &dgpo, hbdAmount)
		objects.PutDGPO(s, &dgpo)
	}
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
func distributeCuratorPool(s *state.Session, author, permlink string, pool int64) int64 {
	c, cerr := objects.GetComment(s, author, permlink)
	if cerr != nil {
		return pool
	}
	if c.TotalVoteWeight == 0 || pool <= 0 {
		return pool
	}
	totalW := new(big.Int).SetUint64(c.TotalVoteWeight)
	poolBig := big.NewInt(pool)

	type pay struct {
		voter  string
		weight uint64
		share  int64
	}
	var pays []pay

	_ = objects.RangeCommentVotesByComment(s, auth.AccountName(author), permlink,
		func(cv *objects.CommentVote) (bool, error) {
			if cv.CurationWeight == 0 {
				return true, nil
			}
			w := new(big.Int).SetUint64(cv.CurationWeight)
			share := new(big.Int).Mul(poolBig, w)
			share.Quo(share, totalW)
			if !share.IsInt64() {
				return true, nil
			}
			s64 := share.Int64()
			if s64 <= 0 {
				return true, nil
			}
			pays = append(pays, pay{voter: string(cv.Voter), weight: cv.CurationWeight, share: s64})
			return true, nil
		})

	// hived's pay_curators iterates a std::set keyed by
	// (weight DESC, voter ASC) — see database_comment.cpp:131-140. Each
	// CreateVesting call mutates total_vesting_fund_hive/total_vesting_shares,
	// so the order curators are paid in subtly affects the share-price
	// each subsequent curator sees. Match the C++ order.
	sort.Slice(pays, func(i, j int) bool {
		if pays[i].weight != pays[j].weight {
			return pays[i].weight > pays[j].weight
		}
		return pays[i].voter < pays[j].voter
	})

	paid := int64(0)
	dgpoNow, _ := objects.GetDGPO(s)
	// HF17_659 (block 10,629,455 mainnet) flips curator vests to the
	// reward_balance — claimable later via claim_reward_balance —
	// instead of crediting vesting_shares directly. Mirrors C++ at
	// database_comment.cpp:188 `create_vesting2(... has_hardfork(HF17_659) ...)`.
	hf := hardfork.SetForBlock(dgpoNow.HeadBlockNumber)
	toReward := hf.Has(hardfork.HF17)
	for _, p := range pays {
		if traceAccount(p.voter) || traceAccount(author) {
			ghiveDiff("GHIVEDIFF CASHOUT-CURATOR block=%d author=%s permlink=%s voter=%s weight=%d total_weight=%d pool=%d claim=%d\n",
				dgpoNow.HeadBlockNumber, author, permlink, p.voter, p.weight, c.TotalVoteWeight, pool, p.share)
		}
		_, _ = CreateVesting_via_chain(s, p.voter, p.share, toReward)
		paid += p.share
	}
	if paid > pool {
		return 0
	}
	return pool - paid
}
