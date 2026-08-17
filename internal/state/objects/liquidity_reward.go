package objects

import (
	"errors"
	"fmt"
	"math/big"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// LiquidityRewardBalance mirrors hive::chain::liquidity_reward_balance_object
// (libraries/chain/include/hive/chain/detail/state/liquidity_reward_balance_object.hpp).
//
// Tracks each account's market-making volume on the limit-order book.
// match() in the matching engine credits the maker (+volume) and debits
// the taker (-volume) on every fill while pre-HIVE_HARDFORK_0_12__178.
// Once per hour pay_liquidity_reward picks the highest-weight account
// and pays them the liquidity reward (≥1200 HIVE).
//
// `weight` is a uint128 (we store as 16-byte big-endian) recomputed
// from the volumes via update_weight():
//
//	pre-HF9:   weight = hive_volume × hbd_volume (only when both > 0)
//	HF9-only:  weight = min(hive_volume, hbd_volume) (only when both > 0)
//	HF10+:     weight = min(hive_volume, hbd_volume) (only when both > 0)
//
// `hive_volume`/`hbd_volume` may go negative (the taker side gets
// debited) but `weight` is zero whenever either volume is non-positive.
type LiquidityRewardBalance struct {
	Owner      auth.AccountName
	LastUpdate uint32
	Weight     [16]byte // big-endian uint128
	HiveVolume int64
	HBDVolume  int64
}

const LiquidityRewardBalanceTable state.TableID = 21

// LiquidityRewardByWeightIndex orders entries by (weight, owner)
// matching hived's by_volume_weight composite key. We sort ASCENDING
// (smallest first) and pay_liquidity_reward iterates BACKWARD from the
// end to find the highest weight; alternatively, by encoding weight
// inverted we could sort the highest first — we keep ascending here so
// the on-disk key is straightforward (raw uint128 BE) and the per-block
// hook does the reverse walk.
const LiquidityRewardByWeightIndex state.IndexID = 1

func liquidityRewardKey(owner auth.AccountName) []byte {
	return state.AppendAccountName(nil, owner)
}

func liquidityRewardByWeightKey(weight [16]byte, owner auth.AccountName) []byte {
	out := make([]byte, 0, 16+state.AccountNameKeyLen)
	out = append(out, weight[:]...)
	return state.AppendAccountName(out, owner)
}

// AppendLiquidityRewardBalance serializes b to dst.
func AppendLiquidityRewardBalance(dst []byte, b *LiquidityRewardBalance) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(b.Owner)))
	dst = append(dst, b.Owner...)
	dst = appendU32LE(dst, b.LastUpdate)
	dst = append(dst, b.Weight[:]...)
	dst = appendI64LE(dst, b.HiveVolume)
	dst = appendI64LE(dst, b.HBDVolume)
	return dst
}

// ReadLiquidityRewardBalance decodes a row.
func ReadLiquidityRewardBalance(src []byte) (LiquidityRewardBalance, error) {
	r := fcraw.NewReader(src)
	var b LiquidityRewardBalance
	owner, err := r.ReadString()
	if err != nil {
		return b, fmt.Errorf("liquidity_reward.owner: %w", err)
	}
	b.Owner = owner
	if b.LastUpdate, err = r.ReadUint32(); err != nil {
		return b, err
	}
	if err := r.ReadInto(b.Weight[:]); err != nil {
		return b, err
	}
	if b.HiveVolume, err = r.ReadInt64(); err != nil {
		return b, err
	}
	if b.HBDVolume, err = r.ReadInt64(); err != nil {
		return b, err
	}
	if !r.AtEnd() {
		return b, errors.New("liquidity_reward: trailing bytes")
	}
	return b, nil
}

// PutLiquidityRewardBalance stores b under the primary key (owner) and
// updates the by-weight secondary index.
func PutLiquidityRewardBalance(s *state.Session, b *LiquidityRewardBalance) {
	s.Put(LiquidityRewardBalanceTable, state.PrimaryIndex, liquidityRewardKey(b.Owner), AppendLiquidityRewardBalance(nil, b))
	s.Put(LiquidityRewardBalanceTable, LiquidityRewardByWeightIndex,
		liquidityRewardByWeightKey(b.Weight, b.Owner), nil)
}

// PutLiquidityRewardBalanceUpdating writes b after first dropping its
// previous by-weight entry — pass the prior weight bytes so the
// secondary index doesn't accumulate stale rows when the weight
// recomputes.
func PutLiquidityRewardBalanceUpdating(s *state.Session, b *LiquidityRewardBalance, prevWeight [16]byte) {
	if prevWeight != b.Weight {
		s.Delete(LiquidityRewardBalanceTable, LiquidityRewardByWeightIndex,
			liquidityRewardByWeightKey(prevWeight, b.Owner))
	}
	PutLiquidityRewardBalance(s, b)
}

// GetLiquidityRewardBalance fetches the row for owner.
func GetLiquidityRewardBalance(s *state.Session, owner auth.AccountName) (LiquidityRewardBalance, error) {
	v, err := s.Get(LiquidityRewardBalanceTable, state.PrimaryIndex, liquidityRewardKey(owner))
	if err != nil {
		return LiquidityRewardBalance{}, err
	}
	return ReadLiquidityRewardBalance(v)
}

// HasLiquidityRewardBalance reports row existence.
func HasLiquidityRewardBalance(s *state.Session, owner auth.AccountName) (bool, error) {
	return s.Has(LiquidityRewardBalanceTable, state.PrimaryIndex, liquidityRewardKey(owner))
}

// DeleteLiquidityRewardBalanceFull removes both indices.
func DeleteLiquidityRewardBalanceFull(s *state.Session, b *LiquidityRewardBalance) {
	s.Delete(LiquidityRewardBalanceTable, state.PrimaryIndex, liquidityRewardKey(b.Owner))
	s.Delete(LiquidityRewardBalanceTable, LiquidityRewardByWeightIndex,
		liquidityRewardByWeightKey(b.Weight, b.Owner))
}

// ComputeLiquidityWeight implements liquidity_reward_balance_object's
// update_weight: HF9+ uses min(hive,hbd); pre-HF9 uses hive×hbd. The
// weight is zero when either volume is non-positive (is_positive() == 0
// in C++).
func ComputeLiquidityWeight(hiveVolume, hbdVolume int64, hf9 bool) [16]byte {
	if hiveVolume <= 0 || hbdVolume <= 0 {
		return [16]byte{}
	}
	var out [16]byte
	if hf9 {
		w := hiveVolume
		if hbdVolume < w {
			w = hbdVolume
		}
		// uint128 big-endian: hi=0, lo=w
		out[8] = byte(w >> 56)
		out[9] = byte(w >> 48)
		out[10] = byte(w >> 40)
		out[11] = byte(w >> 32)
		out[12] = byte(w >> 24)
		out[13] = byte(w >> 16)
		out[14] = byte(w >> 8)
		out[15] = byte(w)
		return out
	}
	prod := new(big.Int).Mul(big.NewInt(hiveVolume), big.NewInt(hbdVolume))
	src := prod.Bytes()
	if len(src) > 16 {
		// saturate to max — should never trigger in practice
		for i := range out {
			out[i] = 0xff
		}
		return out
	}
	copy(out[16-len(src):], src)
	return out
}
