package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/state"
)

// RewardFundTable holds reward_fund_object rows. Created at HF17 with a
// single "post" fund populated from dgpo.total_reward_fund_hive.
const RewardFundTable state.TableID = 21

// CurveID enumerates the reward curves selectable per fund.
type CurveID uint8

const (
	CurveQuadratic            CurveID = 0
	CurveBoundedCurve         CurveID = 1
	CurveLinear               CurveID = 2
	CurveSquareRoot           CurveID = 3
	CurveConvergentLinear     CurveID = 4
	CurveConvergentSquareRoot CurveID = 5
)

// RewardFundName / HIVE_POST_REWARD_FUND_NAME — the sole reward fund
// since HF17. We hardcode the name; key by it.
const PostRewardFundName = "post"

// RewardFund mirrors hived's reward_fund_object
// (libraries/chain/include/hive/chain/detail/state/reward_fund_object_multiindex.hpp).
//
// Behavior:
//   - process_funds at HF16+ adds the per-block content_reward to
//     reward_balance.
//   - cashout_comment_helper draws from reward_balance (and reduces it)
//     in proportion to curve(net_rshares) / recent_claims.
//   - recent_claims accumulates `evaluate_reward_curve(net_rshares)` on
//     every vote and decays exponentially over RecentClaimDecayRate
//     (15 days at HF17, 1 day at HF19+).
type RewardFund struct {
	Name                   string
	RewardBalance          int64    // HIVE-satoshi
	RecentClaims           [16]byte // uint128 BE
	LastUpdate             uint32
	ContentConstant        int64
	PercentCurationRewards uint16 // basis points (0..10000)
	PercentContentRewards  uint16 // basis points
	AuthorRewardCurve      CurveID
	CurationRewardCurve    CurveID
	// DownvoteRewardBalance (HF21+) — HIVE-satoshi pool that the
	// dgpo.DownvotePoolPercent share of inflation is routed to. Drained
	// at cashout in proportion to the comment's downvote weight.
	// Mirrors hived's reward_fund_object_ex::downvote_reward_balance
	// (post-HF21 it lives in a separate companion object; we fold it
	// in here for storage simplicity).
	DownvoteRewardBalance int64
}

func rewardFundKey(name string) []byte {
	return []byte(name)
}

// AppendRewardFund serializes the fund.
func AppendRewardFund(dst []byte, f *RewardFund) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(f.Name)))
	dst = append(dst, f.Name...)
	dst = appendI64LE(dst, f.RewardBalance)
	dst = append(dst, f.RecentClaims[:]...)
	dst = appendU32LE(dst, f.LastUpdate)
	dst = appendI64LE(dst, f.ContentConstant)
	dst = append(dst, byte(f.PercentCurationRewards), byte(f.PercentCurationRewards>>8))
	dst = append(dst, byte(f.PercentContentRewards), byte(f.PercentContentRewards>>8))
	dst = append(dst, byte(f.AuthorRewardCurve))
	dst = append(dst, byte(f.CurationRewardCurve))
	dst = appendI64LE(dst, f.DownvoteRewardBalance)
	return dst
}

// ReadRewardFund decodes a fund from src.
func ReadRewardFund(src []byte) (RewardFund, error) {
	r := fcraw.NewReader(src)
	var f RewardFund
	var err error
	if f.Name, err = r.ReadString(); err != nil {
		return f, fmt.Errorf("reward_fund.name: %w", err)
	}
	if f.RewardBalance, err = r.ReadInt64(); err != nil {
		return f, err
	}
	if err := r.ReadInto(f.RecentClaims[:]); err != nil {
		return f, err
	}
	if f.LastUpdate, err = r.ReadUint32(); err != nil {
		return f, err
	}
	if f.ContentConstant, err = r.ReadInt64(); err != nil {
		return f, err
	}
	if f.PercentCurationRewards, err = r.ReadUint16(); err != nil {
		return f, err
	}
	if f.PercentContentRewards, err = r.ReadUint16(); err != nil {
		return f, err
	}
	var curveAuthor [1]byte
	if err := r.ReadInto(curveAuthor[:]); err != nil {
		return f, err
	}
	f.AuthorRewardCurve = CurveID(curveAuthor[0])
	var curveCur [1]byte
	if err := r.ReadInto(curveCur[:]); err != nil {
		return f, err
	}
	f.CurationRewardCurve = CurveID(curveCur[0])
	// DownvoteRewardBalance is a later schema addition; tolerate EOF.
	if r.AtEnd() {
		return f, nil
	}
	if f.DownvoteRewardBalance, err = r.ReadInt64(); err != nil {
		return f, fmt.Errorf("reward_fund.downvote_reward_balance: %w", err)
	}
	if !r.AtEnd() {
		return f, errors.New("reward_fund: trailing bytes")
	}
	return f, nil
}

// PutRewardFund stores the fund.
func PutRewardFund(s *state.Session, f *RewardFund) {
	s.Put(RewardFundTable, state.PrimaryIndex, rewardFundKey(f.Name), AppendRewardFund(nil, f))
}

// GetRewardFund fetches by name; ErrNotFound if absent.
func GetRewardFund(s *state.Session, name string) (RewardFund, error) {
	v, err := s.Get(RewardFundTable, state.PrimaryIndex, rewardFundKey(name))
	if err != nil {
		return RewardFund{}, err
	}
	return ReadRewardFund(v)
}

// HasRewardFund reports whether a named fund exists.
func HasRewardFund(s *state.Session, name string) (bool, error) {
	return s.Has(RewardFundTable, state.PrimaryIndex, rewardFundKey(name))
}
