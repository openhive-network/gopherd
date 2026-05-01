package chain

import (
	"fmt"
	"math/big"
	"os"
	"sort"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/reward"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

func ghiveDiff(format string, args ...any) {
	if os.Getenv("GHIVEDIFF") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, format, args...)
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
		author      string
		permlink    string
	}
	var ready []pending

	_ = s.Range(objects.CommentCashoutScheduleTable, state.PrimaryIndex, nil,
		func(body, _ []byte) (bool, error) {
			if len(body) < 4+state.AccountNameKeyLen {
				return true, nil
			}
			t := uint32(body[0])<<24 | uint32(body[1])<<16 | uint32(body[2])<<8 | uint32(body[3])
			if t > blockTime {
				return false, nil
			}
			authorBytes := body[4 : 4+state.AccountNameKeyLen]
			permlink := body[4+state.AccountNameKeyLen:]
			n := state.AccountNameKeyLen
			for n > 0 && authorBytes[n-1] == 0 {
				n--
			}
			ready = append(ready, pending{
				cashoutTime: t,
				author:      string(authorBytes[:n]),
				permlink:    string(permlink),
			})
			return true, nil
		})

	curationPct := curationPercentForBlock(blockNum)
	hf := hardfork.SetForBlock(blockNum)
	if hf.Has(hardfork.HF17) {
		// HF17+: pay each due comment individually (matches
		// database_comment.cpp:466-481).
		for _, p := range ready {
			processOneCashout(s, blockTime, blockNum, p.cashoutTime, p.author, p.permlink, curationPct)
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
		// by-root index while iterating. Ordered by (author, permlink)
		// since that's the secondary key — close enough to hived's
		// by_root iteration order for the cascade.
		type sibling struct {
			author      string
			permlink    string
			cashoutTime uint32
		}
		var siblings []sibling
		prefix := objects.CommentByRootPrefix(rootAuthor, rootPermlink)
		_ = s.Range(objects.CommentTable, objects.CommentByRootIndex, prefix,
			func(body, _ []byte) (bool, error) {
				// body = [root_author 16][root_permlink][0x00][author 16][permlink]
				// Strip the root prefix to leave [author 16][permlink].
				if len(body) <= len(prefix) {
					return true, nil
				}
				rest := body[len(prefix):]
				if len(rest) < state.AccountNameKeyLen {
					return true, nil
				}
				authorBytes := rest[:state.AccountNameKeyLen]
				permlink := string(rest[state.AccountNameKeyLen:])
				n := state.AccountNameKeyLen
				for n > 0 && authorBytes[n-1] == 0 {
					n--
				}
				author := string(authorBytes[:n])
				sib, gerr := objects.GetComment(s, author, permlink)
				if gerr != nil {
					return true, nil
				}
				siblings = append(siblings, sibling{
					author:      author,
					permlink:    permlink,
					cashoutTime: sib.CashoutTime,
				})
				return true, nil
			})
		for _, sib := range siblings {
			key := sib.author + "/" + sib.permlink
			if _, dup := paid[key]; dup {
				continue
			}
			paid[key] = struct{}{}
			processOneCashout(s, blockTime, blockNum, sib.cashoutTime, sib.author, sib.permlink, curationPct)
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
func processOneCashout(s *state.Session, blockTime, blockNum, scheduledTime uint32, author, permlink string, curationPct int64) {
	objects.DeleteCommentCashoutSchedule(s, scheduledTime, author, permlink)

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
	defer func() {
		objects.PutComment(s, &c)
		if rescheduleTime != 0 {
			objects.PutCommentCashoutSchedule(s, rescheduleTime, c.Author, c.Permlink)
		}
	}()

	if !c.AllowCurationRewards || c.NetRshares <= 0 {
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

	// payout = fundBalance × curve / shares2.
	payoutBig := new(big.Int).Mul(big.NewInt(fundBalance), commentCurve)
	payoutBig.Quo(payoutBig, shares2)
	if !payoutBig.IsInt64() {
		payoutBig.SetInt64(int64(^uint64(0) >> 1))
	}
	claimed := payoutBig.Int64()
	if claimed > c.MaxAcceptedPayout {
		claimed = c.MaxAcceptedPayout
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
	shares2.Sub(shares2, commentCurve)
	if shares2.Sign() < 0 {
		shares2.SetInt64(0)
	}
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

	// Beneficiaries (HF17+): take their share OFF THE TOP of authorPool.
	// Each beneficiary's slice is paid as if the beneficiary were the
	// author — half via create_hbd, half via create_vesting.
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

	// Distribute curators using the configured curve weights when
	// using the reward_fund_object — otherwise the simple rshares²
	// fallback. Pre-HF20, the unclaimed remainder (pool minus actual
	// curator payouts — non-zero because TotalVoteWeight accumulates
	// undiscounted weights while individual votes carry the
	// reverse-auction-discounted weight) is forwarded back to the
	// author. HF20 (block ~25M) flips this off — unclaimed evaporates.
	curatorRemainder := int64(0)
	if curatorPool > 0 {
		curatorRemainder = distributeCuratorPool(s, c.Author, c.Permlink, curatorPool)
	}
	hf := hardfork.SetForBlock(blockNum)
	if !hf.Has(hardfork.HF20) && curatorRemainder > 0 {
		authorPool += curatorRemainder
	}

	// Author payout split: half HBD-style (subject to print rate) +
	// half VESTS at the dynamic price.
	hbdHive := authorPool * int64(c.PercentHbd) / 20000
	vestingHive := authorPool - hbdHive
	if author == "proskynneo" || author == "hipster" {
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

	// Drop all CommentVote rows for this comment — hived's cashout
	// loop (libraries/chain/database_comment.cpp:366-373) removes them
	// to free memory and so any post-cashout re-vote starts fresh. Our
	// vote evaluator now also gates votes on CashoutTime==0 comments,
	// so this cleanup is mostly a memory hygiene measure.
	deleteCommentVotes(s, author, permlink)
}

// deleteCommentVotes walks the by-comment secondary index for the
// (author, permlink) comment and removes each vote row.
func deleteCommentVotes(s *state.Session, author, permlink string) {
	type voterKey struct{ voter string }
	var votes []voterKey
	prefix := objects.CommentVoteByCommentPrefix(author, permlink)
	_ = s.Range(objects.CommentVoteTable, objects.CommentVoteByCommentIndex, prefix,
		func(body, _ []byte) (bool, error) {
			if len(body) < state.AccountNameKeyLen {
				return true, nil
			}
			vb := body[len(body)-state.AccountNameKeyLen:]
			n := state.AccountNameKeyLen
			for n > 0 && vb[n-1] == 0 {
				n--
			}
			votes = append(votes, voterKey{voter: string(vb[:n])})
			return true, nil
		})
	for _, v := range votes {
		objects.DeleteCommentVote(s, v.voter, author, permlink)
	}
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
	case objects.CurveConvergentLinear:
		// rshares² / (2s + rshares)
		num := new(big.Int).Mul(r, r)
		den := new(big.Int).Add(new(big.Int).Lsh(s, 1), r)
		return new(big.Int).Quo(num, den)
	case objects.CurveConvergentSquareRoot:
		// rshares / (s + sqrt(rshares))
		num := new(big.Int).Set(r)
		den := new(big.Int).Add(s, new(big.Int).Sqrt(r))
		return new(big.Int).Quo(num, den)
	default:
		// quadratic / bounded_curve: (r+s)² - s²
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

	// adjust_supply: -toHbdHive HIVE, +hbdAmount HBD.
	if dgpo, derr := objects.GetDGPO(s); derr == nil {
		dgpo.CurrentSupply -= toHbdHive
		dgpo.CurrentHBDSupply += hbdAmount
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

	prefix := objects.CommentVoteByCommentPrefix(author, permlink)
	_ = s.Range(objects.CommentVoteTable, objects.CommentVoteByCommentIndex, prefix,
		func(body, _ []byte) (bool, error) {
			if len(body) < state.AccountNameKeyLen {
				return true, nil
			}
			voterBytes := body[len(body)-state.AccountNameKeyLen:]
			n := state.AccountNameKeyLen
			for n > 0 && voterBytes[n-1] == 0 {
				n--
			}
			voter := string(voterBytes[:n])
			cv, gerr := objects.GetCommentVote(s, voter, author, permlink)
			if gerr != nil || cv.CurationWeight == 0 {
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
			pays = append(pays, pay{voter: voter, weight: cv.CurationWeight, share: s64})
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
	for _, p := range pays {
		if p.voter == "proskynneo" || p.voter == "hipster" {
			ghiveDiff("GHIVEDIFF CASHOUT-CURATOR block=%d author=%s permlink=%s voter=%s weight=%d total_weight=%d pool=%d claim=%d\n",
				dgpoNow.HeadBlockNumber, author, permlink, p.voter, p.weight, c.TotalVoteWeight, pool, p.share)
		}
		_, _ = CreateVesting_via_chain(s, p.voter, p.share, false)
		paid += p.share
	}
	if paid > pool {
		return 0
	}
	return pool - paid
}
