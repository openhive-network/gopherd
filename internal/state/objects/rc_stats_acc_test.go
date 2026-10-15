package objects

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"gopherd/internal/state"
)

// TestRCStatsAccumulateAndFold drives rc_stats_object::add_stats through the
// side rows and checks the folded report: op bins (single op by tag, the rest
// in multiop), payer rank and low-mana counters, and the stamp as SHA-256
// over each payer's mana, little-endian.
func TestRCStatsAccumulateAndFold(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := state.NewSession(db)
	PutRCStats(s, RCStatsIDPending, &RCStats{BlockNum: 100, AverageCost: [3]int64{1000, 0, 0}})

	txs := []RCTxInfo{
		{Op: 0, Cost: [RCNumResources]int64{10, 0, 0, 5, 5}, Usage: [RCNumResources]int64{100}, Max: 5_000_000_000_000, RC: 4_000_000_000_000},
		{Op: 0, Cost: [RCNumResources]int64{30, 0, 0, 5, 5}, Max: 5_000_000_000_000, RC: 100},
		{Op: -1, Cost: [RCNumResources]int64{7}, Max: 0, RC: 0},
	}
	h := sha256.New()
	for i := range txs {
		AddRCStats(s, &txs[i])
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(txs[i].RC))
		h.Write(b[:])
	}
	p, err := GetRCStats(s, RCStatsIDPending)
	if err != nil {
		t.Fatal(err)
	}
	FoldRCStats(s, &p)

	if v := p.OpStats[0]; v.Count != 2 || v.Cost[0] != 40 || v.Usage[0] != 100 {
		t.Fatalf("vote bin = %+v", v)
	}
	if v := p.OpStats[RCNumOperations]; v.Count != 1 || v.Cost[0] != 7 {
		t.Fatalf("multiop bin = %+v", v)
	}
	// log10(5e12) - 9 = 3.
	ps := p.PayerStats[3]
	if ps.Count != 2 || ps.LessThan5Percent != 1 || ps.LessThan20Percent != 1 || ps.CantAfford[0] != 1 {
		t.Fatalf("payer rank 3 = %+v", ps)
	}
	var want [32]byte
	copy(want[:], h.Sum(nil))
	if p.Stamp != want {
		t.Fatalf("stamp %x, want %x", p.Stamp, want)
	}
	// The side rows are consumed.
	if has, _ := s.Has(RCStatsTable, state.PrimaryIndex, rcStatsOpKey(0)); has {
		t.Fatal("op side row left behind after fold")
	}
	if has, _ := s.Has(RCStatsTable, state.PrimaryIndex, rcStatsStampKey); has {
		t.Fatal("stamp side row left behind after fold")
	}
}

func TestRCPayerRank(t *testing.T) {
	for _, c := range []struct {
		max  int64
		want int
	}{{1, 0}, {999_999_999, 0}, {1_000_000_000, 0}, {10_000_000_000, 1}, {99_999_999_999, 1}, {1 << 62, 7}} {
		if got := rcPayerRank(c.max); got != c.want {
			t.Errorf("rank(%d) = %d, want %d", c.max, got, c.want)
		}
	}
}
