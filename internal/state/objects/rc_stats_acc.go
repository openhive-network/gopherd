package objects

import (
	"crypto/sha256"
	"encoding"
	"encoding/binary"

	"gopherd/internal/fcraw"
	"gopherd/internal/state"
)

// Per-transaction rc_stats accumulation (rc_stats_object::add_stats,
// rc_objects.cpp:30).
//
// hived updates the pending rc_stats_object in memory for every transaction.
// Here the pending row is a ~5 KB record, and decoding plus re-encoding it per
// transaction would cost a replay ~20%, so the per-transaction counters live in
// small side rows of the same table instead, keyed under the pending id:
//
//	{0,'o',tag}   RCOpStats of one op tag (RCNumOperations = multiop)
//	{0,'p',rank}  RCPayerStats of one payer rank
//	{0,'s'}       running SHA-256 state of the stamp
//	{0,'a'}       the pending row's AverageCost, read by the payer filter
//
// FoldRCStats moves them into the pending row at the report boundary. The
// two-row codec, and so every existing snapshot, is unchanged.

func rcStatsOpKey(tag int) []byte     { return []byte{RCStatsIDPending, 'o', byte(tag)} }
func rcStatsPayerKey(rank int) []byte { return []byte{RCStatsIDPending, 'p', byte(rank)} }

var (
	rcStatsStampKey   = []byte{RCStatsIDPending, 's'}
	rcStatsAvgCostKey = []byte{RCStatsIDPending, 'a'}
)

// RCTxInfo mirrors rc_transaction_info (rc_export_objects.hpp:15).
type RCTxInfo struct {
	Usage [RCNumResources]int64
	Cost  [RCNumResources]int64
	// Max is the payer's maximum RC before regeneration, 0 when the
	// transaction has no payer.
	Max int64
	// RC is the payer's mana after the charge (rc_utility.cpp:605).
	RC int64
	// Op is the operation tag of a single-operation transaction, -1 for a
	// multi-operation one.
	Op int
}

// AddRCStats folds one transaction into the pending stats.
func AddRCStats(s *state.Session, info *RCTxInfo) {
	addRCStamp(s, info.RC)

	idx := RCNumOperations
	if info.Op >= 0 && info.Op < RCNumOperations {
		idx = info.Op
	}
	os := getRCOpStatsAcc(s, idx)
	os.Count++
	for i := 0; i < RCNumResources; i++ {
		os.Cost[i] += info.Cost[i]
		os.Usage[i] += info.Usage[i]
	}
	s.Put(RCStatsTable, state.PrimaryIndex, rcStatsOpKey(idx), appendOpStats(nil, &os))

	if info.Max <= 0 {
		return
	}
	rank := rcPayerRank(info.Max)
	ps := getRCPayerStatsAcc(s, rank)
	ps.Count++
	for i := 0; i < RCNumResources; i++ {
		ps.Cost[i] += info.Cost[i]
		ps.Usage[i] += info.Usage[i]
	}
	low := info.Max / 20 // 5%
	if info.RC < low {
		ps.LessThan5Percent++
	}
	low <<= 1 // 10%
	if info.RC < low {
		ps.LessThan10Percent++
	}
	low <<= 1 // 20%
	if info.RC < low {
		ps.LessThan20Percent++
	}
	avg := rcStatsAverageCosts(s)
	for i := 0; i < 3; i++ {
		if info.RC < avg[i] {
			ps.CantAfford[i]++
		}
	}
	s.Put(RCStatsTable, state.PrimaryIndex, rcStatsPayerKey(rank), appendPayerStats(nil, &ps))
}

// rcPayerRank is `log10( tx_info.max ) - 9` clamped to the rank range.
func rcPayerRank(max int64) int {
	digits := 0
	for v := max; v > 0; v /= 10 {
		digits++
	}
	r := digits - 1 - 9
	if r < 0 {
		r = 0
	}
	if r >= RCNumPayerRanks {
		r = RCNumPayerRanks - 1
	}
	return r
}

func getRCOpStatsAcc(s *state.Session, idx int) RCOpStats {
	v, err := s.Get(RCStatsTable, state.PrimaryIndex, rcStatsOpKey(idx))
	if err != nil {
		return RCOpStats{}
	}
	os, err := readOpStats(fcraw.NewReader(v))
	if err != nil {
		return RCOpStats{}
	}
	return os
}

func getRCPayerStatsAcc(s *state.Session, rank int) RCPayerStats {
	v, err := s.Get(RCStatsTable, state.PrimaryIndex, rcStatsPayerKey(rank))
	if err != nil {
		return RCPayerStats{}
	}
	ps, err := readPayerStats(fcraw.NewReader(v))
	if err != nil {
		return RCPayerStats{}
	}
	return ps
}

// rcStatsAverageCosts returns the pending row's AverageCost, caching it in
// its side row the first time (rows written before this file existed have
// none).
func rcStatsAverageCosts(s *state.Session) [3]int64 {
	var avg [3]int64
	if v, err := s.Get(RCStatsTable, state.PrimaryIndex, rcStatsAvgCostKey); err == nil && len(v) == 24 {
		for i := range avg {
			avg[i] = int64(binary.LittleEndian.Uint64(v[8*i:]))
		}
		return avg
	}
	if p, err := GetRCStats(s, RCStatsIDPending); err == nil {
		avg = p.AverageCost
	}
	putRCStatsAverageCosts(s, avg)
	return avg
}

func putRCStatsAverageCosts(s *state.Session, avg [3]int64) {
	b := make([]byte, 0, 24)
	for _, a := range avg {
		b = appendI64LE(b, a)
	}
	s.Put(RCStatsTable, state.PrimaryIndex, rcStatsAvgCostKey, b)
}

// addRCStamp feeds the payer's mana into the rolling stamp
// (stamp.update( &tx_info.rc, sizeof( tx_info.rc ) ), little-endian int64).
func addRCStamp(s *state.Session, rc int64) {
	h := sha256.New()
	if v, err := s.Get(RCStatsTable, state.PrimaryIndex, rcStatsStampKey); err == nil {
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(v); err != nil {
			h.Reset()
		}
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(rc))
	h.Write(b[:])
	st, _ := h.(encoding.BinaryMarshaler).MarshalBinary()
	s.Put(RCStatsTable, state.PrimaryIndex, rcStatsStampKey, st)
}

// FoldRCStats moves the side-row accumulators into p, closes the stamp
// (finalize_stats) and deletes the side rows.
func FoldRCStats(s *state.Session, p *RCStats) {
	for i := 0; i <= RCNumOperations; i++ {
		if has, _ := s.Has(RCStatsTable, state.PrimaryIndex, rcStatsOpKey(i)); has {
			p.OpStats[i] = getRCOpStatsAcc(s, i)
			s.Delete(RCStatsTable, state.PrimaryIndex, rcStatsOpKey(i))
		}
	}
	for i := 0; i < RCNumPayerRanks; i++ {
		if has, _ := s.Has(RCStatsTable, state.PrimaryIndex, rcStatsPayerKey(i)); has {
			p.PayerStats[i] = getRCPayerStatsAcc(s, i)
			s.Delete(RCStatsTable, state.PrimaryIndex, rcStatsPayerKey(i))
		}
	}
	h := sha256.New()
	if v, err := s.Get(RCStatsTable, state.PrimaryIndex, rcStatsStampKey); err == nil {
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(v); err != nil {
			h.Reset()
		}
		s.Delete(RCStatsTable, state.PrimaryIndex, rcStatsStampKey)
	}
	copy(p.Stamp[:], h.Sum(nil))
}

// ResetRCStats starts the next reporting window (reset_stats,
// rc_objects.cpp:13): the window's starting block, the global regen and the
// pool as they are now, and the vote/comment/transfer average costs of the
// window just closed where it saw any.
func ResetRCStats(s *state.Session, closed *RCStats, blockNum uint32, regen int64, pool *RCPool) {
	next := RCStats{BlockNum: blockNum, Regen: regen, AverageCost: closed.AverageCost}
	next.Budget = pool.LastKnownBudget
	next.Pool = pool.PoolArray
	div := pool.SumOfResourceWeights
	if div == 0 {
		div = 1
	}
	for i := 0; i < RCNumResources; i++ {
		next.Share[i] = uint16(pool.ResourceWeights[i] * 10000 / div)
	}
	for i := 0; i < 3; i++ {
		if os := &closed.OpStats[i]; os.Count > 0 {
			var sum int64
			for j := 0; j < RCNumResources; j++ {
				sum += os.Cost[j]
			}
			next.AverageCost[i] = sum / int64(os.Count)
		}
	}
	PutRCStats(s, RCStatsIDPending, &next)
	putRCStatsAverageCosts(s, next.AverageCost)
}
