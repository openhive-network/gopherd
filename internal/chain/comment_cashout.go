package chain

import (
	"math/big"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/reward"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

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
// Mirrors libraries/chain/database_comment.cpp::cashout_comment_helper,
// applied in time order against the per-block schedule index. blockNum
// drives the curation/author split (HF8 toggles 50/50 → 25/75).
func processCommentCashouts(s *state.Session, blockTime, blockNum uint32) {
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
	for _, p := range ready {
		processOneCashout(s, p.cashoutTime, p.author, p.permlink, curationPct)
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
func processOneCashout(s *state.Session, scheduledTime uint32, author, permlink string, curationPct int64) {
	objects.DeleteCommentCashoutSchedule(s, scheduledTime, author, permlink)

	c, err := objects.GetComment(s, author, permlink)
	if err != nil {
		return
	}
	c.CashoutTime = 0
	defer objects.PutComment(s, &c)

	if !c.AllowCurationRewards || c.NetRshares <= 0 {
		return
	}

	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return
	}
	if dgpo.TotalRewardFundHive <= 0 {
		return
	}

	totalShares2 := reward.Uint128FromBytes(dgpo.TotalRewardShares2)
	if totalShares2.Sign() <= 0 {
		return
	}
	commentCurve := reward.EvaluateRewardCurveQuadratic(c.NetRshares)
	if commentCurve.Sign() <= 0 {
		return
	}

	// payout = reward_fund × curve / total_shares2.
	payoutBig := new(big.Int).Mul(big.NewInt(dgpo.TotalRewardFundHive), commentCurve)
	payoutBig.Quo(payoutBig, totalShares2)
	if !payoutBig.IsInt64() {
		// Cap to int64 max if anything pathologically large slipped through.
		payoutBig.SetInt64(int64(^uint64(0) >> 1))
	}
	claimed := payoutBig.Int64()
	if claimed > c.MaxAcceptedPayout {
		claimed = c.MaxAcceptedPayout
	}
	if claimed > dgpo.TotalRewardFundHive {
		claimed = dgpo.TotalRewardFundHive
	}

	// adjust_rshares2: drop this comment's curve from the global sum
	// regardless of whether the payout was zero (it still removes the
	// claim weight from the running total).
	totalShares2.Sub(totalShares2, commentCurve)
	if totalShares2.Sign() < 0 {
		totalShares2.SetInt64(0)
	}
	dgpo.TotalRewardShares2 = reward.Uint128ToBytes(totalShares2)

	if claimed <= 0 {
		objects.PutDGPO(s, &dgpo)
		return
	}
	dgpo.TotalRewardFundHive -= claimed
	objects.PutDGPO(s, &dgpo)

	curatorPool := claimed * curationPct / 10000
	authorPool := claimed - curatorPool

	// pay_curators: distribute curatorPool by each voter's rshares²
	// (matching hived's by-comment-vote-weight ordering since our weight
	// equals rshares² in this simplified model).
	if curatorPool > 0 {
		distributeCuratorPool(s, c.Author, c.Permlink, curatorPool)
	}

	// Author payout. hived's create_hbd splits the HIVE-denominated
	// reward into hbd_payout + hive_payout based on the median price
	// feed; with no feed (early chain), the whole amount is HIVE
	// balance. percent_hbd / 20000 of the author pool goes through
	// create_hbd; the remainder is paid as VESTS via CreateVesting.
	hbdHive := authorPool * int64(c.PercentHbd) / 20000
	vestingHive := authorPool - hbdHive
	if hbdHive > 0 {
		if a, gerr := objects.GetAccount(s, author); gerr == nil {
			payCreateHbd(s, &a, hbdHive)
			objects.PutAccount(s, &a)
		}
	}
	if vestingHive > 0 {
		_, _ = CreateVesting_via_chain(s, author, vestingHive, false)
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
func payCreateHbd(s *state.Session, a *objects.Account, hiveAmount int64) {
	if hiveAmount <= 0 {
		return
	}
	fh, err := objects.GetFeedHistory(s)
	if err != nil || priceIsZero(fh.CurrentMedianHistory) {
		// No feed → everything pays as liquid HIVE.
		a.Balance += hiveAmount
		return
	}
	// hbd_print_rate default = HIVE_100_PERCENT (10000); to_hbd = hive
	// (entire HIVE amount converts to HBD via the feed). HF14 lowers
	// this when the chain's HBD supply hits HIVE_HBD_HARD_LIMIT.
	const hbdPrintRate int64 = 10000
	toHbdHive := hiveAmount * hbdPrintRate / 10000
	toHive := hiveAmount - toHbdHive

	hbdAmount := convertHiveToHBD(toHbdHive, fh.CurrentMedianHistory)
	a.HBDBalance += hbdAmount
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
// proportional share of the pool (weighted by rshares²).
func distributeCuratorPool(s *state.Session, author, permlink string, pool int64) {
	type voterShare struct {
		voter string
		w     *big.Int // rshares²
	}
	var voters []voterShare
	totalW := new(big.Int)

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
			if gerr != nil {
				return true, nil
			}
			if cv.Rshares <= 0 {
				return true, nil
			}
			r := big.NewInt(cv.Rshares)
			w := new(big.Int).Mul(r, r)
			voters = append(voters, voterShare{voter: voter, w: w})
			totalW.Add(totalW, w)
			return true, nil
		})

	if totalW.Sign() <= 0 || len(voters) == 0 {
		return
	}

	poolBig := big.NewInt(pool)
	for _, vs := range voters {
		share := new(big.Int).Mul(poolBig, vs.w)
		share.Quo(share, totalW)
		if !share.IsInt64() {
			continue
		}
		s64 := share.Int64()
		if s64 <= 0 {
			continue
		}
		_, _ = CreateVesting_via_chain(s, vs.voter, s64, false)
	}
}
