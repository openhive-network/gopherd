package chain

import (
	"math/big"

	"hived-go/internal/reward"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Author/curator split (basis points of HIVE_100_PERCENT).
//
// hived's defaults are 75/25 pre-HF17, then 50/50 from HF17 onwards
// (per get_curation_rewards_percent in database.cpp). HF17 sits at
// block 10,629,455 — well past the section we're driving forward
// today, so we keep 75/25.
const (
	authorRewardPercent  int64 = 7500
	curatorRewardPercent int64 = 2500
)

// processCommentCashouts pays out comments whose CashoutTime has elapsed.
// Mirrors libraries/chain/database_comment.cpp::cashout_comment_helper,
// applied in time order against the per-block schedule index.
func processCommentCashouts(s *state.Session, blockTime uint32) {
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

	for _, p := range ready {
		processOneCashout(s, p.cashoutTime, p.author, p.permlink)
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
func processOneCashout(s *state.Session, scheduledTime uint32, author, permlink string) {
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

	curatorPool := claimed * curatorRewardPercent / 10000
	authorPool := claimed - curatorPool

	// pay_curators: distribute curatorPool by each voter's rshares²
	// (matching hived's by-comment-vote-weight ordering since our weight
	// equals rshares² in this simplified model).
	if curatorPool > 0 {
		distributeCuratorPool(s, c.Author, c.Permlink, curatorPool)
	}

	// Author payout: percent_hbd / 20000 share goes to liquid HIVE
	// balance (hived's create_hbd path before the feed history is
	// established), remainder via CreateVesting.
	hbdHive := authorPool * int64(c.PercentHbd) / 20000
	vestingHive := authorPool - hbdHive
	if hbdHive > 0 {
		if a, gerr := objects.GetAccount(s, author); gerr == nil {
			a.Balance += hbdHive
			objects.PutAccount(s, &a)
		}
	}
	if vestingHive > 0 {
		_, _ = CreateVesting_via_chain(s, author, vestingHive, false)
	}
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
